//! gna-rdp — passerelle RDP de Gná.
//!
//! Gná lance ce programme pour chaque session RDP :
//!
//!   1. Gná écrit les paramètres (JSON, une ligne) sur l'entrée standard ;
//!   2. la passerelle se connecte en RDP (NLA ou TLS), en vérifiant l'empreinte du
//!      certificat de la cible, puis écrit une ligne d'état JSON sur la sortie d'erreur ;
//!   3. l'entrée et la sortie standard deviennent un flux VNC (RFB) : Gná le relaie
//!      vers noVNC dans le navigateur, et l'enregistre comme une session VNC.
//!
//! Les identifiants ne quittent donc jamais le serveur Gná.
//!
//! `gna-rdp probe <hôte> <port>` affiche l'empreinte du certificat RDP de la cible.

mod clip;
mod drive;
mod keys;
mod rfb;

use std::io::Write as _;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use ironrdp::cliprdr::CliprdrClient;
use ironrdp::connector::{self, ClientConnector, ConnectionResult, Credentials, ServerName};
use ironrdp::displaycontrol::client::DisplayControlClient;
use ironrdp::dvc::DrdynvcClient;
use ironrdp::rdpdr::Rdpdr;
use ironrdp::input::{Database, MouseButton, MousePosition, Operation, WheelRotations};
use ironrdp::pdu::gcc::KeyboardType;
use ironrdp::pdu::rdp::capability_sets::MajorPlatformType;
use ironrdp::pdu::rdp::client_info::{PerformanceFlags, TimezoneInfo};
use ironrdp::session::image::DecodedImage;
use ironrdp::session::{ActiveStage, ActiveStageBuilder, ActiveStageOutput};
use ironrdp_tokio::{FramedWrite as _, TokioFramed};
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, ServerName as TlsName, UnixTime};
use rustls::{DigitallySignedStruct, SignatureScheme};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::net::TcpStream;
use tokio::sync::mpsc;
use tokio_rustls::client::TlsStream;

type Framed = TokioFramed<TlsStream<TcpStream>>;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);

#[derive(Deserialize)]
struct Params {
    host: String,
    port: u16,
    username: String,
    password: String,
    #[serde(default)]
    domain: Option<String>,
    width: u16,
    height: u16,
    fingerprint: String,
    #[serde(default)]
    layout: String,
    #[serde(default)]
    title: String,
    /// Presse-papiers autorisé.
    #[serde(default)]
    clipboard: bool,
    /// Dossier de transfert partagé avec la machine distante (lecteur « Gna »).
    #[serde(default)]
    drive_dir: Option<String>,
    #[serde(default = "default_drive_max")]
    drive_max_mb: u64,
}

fn default_drive_max() -> u64 {
    1024
}

/// Ligne d'état envoyée à Gná sur la sortie d'erreur.
#[derive(Serialize)]
#[serde(tag = "status", rename_all = "snake_case")]
enum Status {
    Ready { width: u16, height: u16 },
    Error { kind: &'static str, message: String },
    Fingerprint { fingerprint: String },
}

fn report(s: &Status) {
    let mut e = std::io::stderr().lock();
    let _ = writeln!(e, "{}", serde_json::to_string(s).unwrap());
    let _ = e.flush();
}

#[derive(Debug)]
enum Failure {
    Network(String),
    Cert(String),
    Auth(String),
    Protocol(String),
}

impl Failure {
    fn status(self) -> Status {
        match self {
            Failure::Network(m) => Status::Error { kind: "network", message: m },
            Failure::Cert(fp) => Status::Error {
                kind: "cert",
                message: format!(
                    "le certificat RDP de la cible ne correspond pas à celui enregistré : connexion refusée \
                     (possible interception). Empreinte présentée : {fp}"
                ),
            },
            Failure::Auth(m) => Status::Error { kind: "auth", message: m },
            Failure::Protocol(m) => Status::Error { kind: "protocol", message: m },
        }
    }
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().expect("runtime tokio");
    let code = match args.get(1).map(String::as_str) {
        Some("probe") if args.len() == 4 => {
            let port = args[3].parse().unwrap_or(3389);
            match rt.block_on(probe(&args[2], port)) {
                Ok(fp) => {
                    report(&Status::Fingerprint { fingerprint: fp });
                    0
                }
                Err(e) => {
                    report(&e.status());
                    1
                }
            }
        }
        Some("bridge") => rt.block_on(bridge()),
        _ => {
            eprintln!("usage : gna-rdp bridge   (paramètres JSON sur l'entrée standard)\n        gna-rdp probe <hôte> <port>");
            2
        }
    };
    std::process::exit(code);
}

// ---------------------------------------------------------------------------
// Connexion RDP
// ---------------------------------------------------------------------------

fn fingerprint(der: &[u8]) -> String {
    Sha256::digest(der).iter().map(|b| format!("{b:02X}")).collect::<Vec<_>>().join(":")
}

fn normalize(fp: &str) -> String {
    fp.chars().filter(|c| c.is_ascii_hexdigit()).collect::<String>().to_ascii_uppercase()
}

/// Vérificateur TLS : relève le certificat et, si une empreinte est épinglée, exige
/// qu'il corresponde. Les signatures de la poignée de main TLS restent vérifiées.
#[derive(Debug)]
struct Pin {
    pinned: Option<String>,
    seen: Mutex<Option<Vec<u8>>>,
    provider: Arc<rustls::crypto::CryptoProvider>,
}

impl ServerCertVerifier for Pin {
    fn verify_server_cert(
        &self,
        cert: &CertificateDer<'_>,
        _: &[CertificateDer<'_>],
        _: &TlsName<'_>,
        _: &[u8],
        _: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        *self.seen.lock().unwrap() = Some(cert.as_ref().to_vec());
        match &self.pinned {
            Some(p) if *p != normalize(&fingerprint(cert.as_ref())) => Err(rustls::Error::General("CERT_MISMATCH".into())),
            _ => Ok(ServerCertVerified::assertion()),
        }
    }
    fn verify_tls12_signature(&self, m: &[u8], c: &CertificateDer<'_>, d: &DigitallySignedStruct) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(m, c, d, &self.provider.signature_verification_algorithms)
    }
    fn verify_tls13_signature(&self, m: &[u8], c: &CertificateDer<'_>, d: &DigitallySignedStruct) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(m, c, d, &self.provider.signature_verification_algorithms)
    }
    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider.signature_verification_algorithms.supported_schemes()
    }
}

/// Bascule TLS de la connexion RDP. Renvoie le flux chiffré, la clé publique du serveur
/// (nécessaire à l'authentification NLA) et l'empreinte du certificat.
async fn tls_upgrade(tcp: TcpStream, host: &str, pinned: Option<&str>) -> Result<(TlsStream<TcpStream>, Vec<u8>, String), Failure> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let pin = Arc::new(Pin { pinned: pinned.map(normalize), seen: Mutex::new(None), provider: provider.clone() });
    let mut config = rustls::ClientConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .map_err(|e| Failure::Protocol(e.to_string()))?
        .dangerous()
        .with_custom_certificate_verifier(pin.clone())
        .with_no_client_auth();
    config.resumption = rustls::client::Resumption::disabled();
    let name = TlsName::try_from(host.to_string()).map_err(|_| Failure::Protocol("nom d'hôte invalide".into()))?;
    let result = tokio::time::timeout(CONNECT_TIMEOUT, tokio_rustls::TlsConnector::from(Arc::new(config)).connect(name, tcp)).await;
    let seen = pin.seen.lock().unwrap().clone();
    let stream = match result {
        Err(_) => return Err(Failure::Network("délai dépassé pendant la poignée de main TLS".into())),
        Ok(Err(e)) if e.to_string().contains("CERT_MISMATCH") => {
            return Err(Failure::Cert(seen.map(|d| fingerprint(&d)).unwrap_or_default()))
        }
        Ok(Err(e)) => return Err(Failure::Protocol(format!("échec TLS : {e}"))),
        Ok(Ok(s)) => s,
    };
    let der = seen.ok_or_else(|| Failure::Protocol("certificat du serveur absent".into()))?;
    let key = server_public_key(&der).ok_or_else(|| Failure::Protocol("clé publique du certificat illisible".into()))?;
    Ok((stream, key, fingerprint(&der)))
}

fn server_public_key(der: &[u8]) -> Option<Vec<u8>> {
    use x509_cert::der::Decode as _;
    let cert = x509_cert::Certificate::from_der(der).ok()?;
    cert.tbs_certificate.subject_public_key_info.subject_public_key.as_bytes().map(<[u8]>::to_vec)
}

/// Client réseau pour Kerberos : non utilisé (authentification NTLM uniquement).
struct NoKerberos;

impl ironrdp_tokio::NetworkClient for NoKerberos {
    async fn send(&mut self, _: &ironrdp::connector::sspi::generator::NetworkRequest) -> connector::ConnectorResult<Vec<u8>> {
        Err(connector::general_err!("Kerberos non pris en charge"))
    }
}

fn rdp_config(p: &Params, layout: keys::Layout) -> connector::Config {
    // « DOMAINE\utilisateur » est accepté en plus du champ domaine.
    let (domain, username) = match p.username.split_once('\\') {
        Some((d, u)) if p.domain.as_deref().unwrap_or("").is_empty() => (Some(d.to_string()), u.to_string()),
        _ => (p.domain.clone().filter(|d| !d.is_empty()), p.username.clone()),
    };
    connector::Config {
        credentials: Credentials::UsernamePassword { username, password: p.password.clone() },
        domain,
        enable_tls: true,
        enable_credssp: true,
        keyboard_type: KeyboardType::IbmEnhanced,
        keyboard_subtype: 0,
        keyboard_layout: layout.windows_id(),
        keyboard_functional_keys_count: 12,
        ime_file_name: String::new(),
        dig_product_id: String::new(),
        desktop_size: connector::DesktopSize { width: p.width, height: p.height },
        bitmap: None,
        client_build: 0,
        client_name: "GNA".to_owned(),
        client_dir: "C:\\Windows\\System32\\mstscax.dll".to_owned(),
        platform: MajorPlatformType::UNIX,
        enable_server_pointer: true,
        pointer_software_rendering: true, // le curseur est dessiné dans l'image (visible aussi à la relecture)
        request_data: None,
        autologon: true,
        enable_audio_playback: false,
        // Pas de compression globale : celle d'xrdp 0.10 (MPPC sur les mises à jour lentes)
        // est mal décodée par IronRDP. Les images restent compressées par leurs propres codecs.
        compression_type: None,
        multitransport_flags: None,
        performance_flags: PerformanceFlags::DISABLE_WALLPAPER
            | PerformanceFlags::DISABLE_FULLWINDOWDRAG
            | PerformanceFlags::DISABLE_MENUANIMATIONS,
        desktop_scale_factor: 0,
        hardware_id: None,
        license_cache: None,
        timezone_info: TimezoneInfo::default(),
        alternate_shell: String::new(),
        work_dir: String::new(),
    }
}

async fn tcp_connect(host: &str, port: u16) -> Result<TcpStream, Failure> {
    let tcp = tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect((host, port)))
        .await
        .map_err(|_| Failure::Network(format!("cible injoignable ({host}:{port}) : délai dépassé")))?
        .map_err(|e| Failure::Network(format!("cible injoignable ({host}:{port}) : {e}")))?;
    tcp.set_nodelay(true).ok();
    Ok(tcp)
}

async fn connect(
    p: &Params,
    layout: keys::Layout,
    clip_tx: tokio::sync::mpsc::UnboundedSender<clip::Event>,
) -> Result<(ConnectionResult, Framed), Failure> {
    let tcp = tcp_connect(&p.host, p.port).await?;
    let client_addr = tcp.local_addr().map_err(|e| Failure::Network(e.to_string()))?;
    let mut framed = TokioFramed::new(tcp);
    let mut connector = ClientConnector::new(rdp_config(p, layout), client_addr);
    // Canaux : redimensionnement (Display Control), presse-papiers, lecteur partagé.
    connector.attach_static_channel(DrdynvcClient::new().with_dynamic_channel(DisplayControlClient::new(|_| Ok(Vec::new()))));
    if p.clipboard {
        connector.attach_static_channel(CliprdrClient::new(Box::new(clip::Backend::new(clip_tx))));
    }
    if let Some(dir) = p.drive_dir.as_deref().filter(|d| !d.is_empty()) {
        let backend = drive::Drive::new(dir.into(), p.drive_max_mb.max(1) * 1024 * 1024);
        connector.attach_static_channel(Rdpdr::new(Box::new(backend), "GNA".into()).with_drives(Some(vec![(1, "Gna".into())])));
    }

    let should_upgrade = tokio::time::timeout(CONNECT_TIMEOUT, ironrdp_tokio::connect_begin(&mut framed, &mut connector))
        .await
        .map_err(|_| Failure::Network("pas de réponse RDP de la cible".into()))?
        .map_err(|e| Failure::Protocol(format!("négociation RDP : {}", e.report())))?;

    let tcp = framed.into_inner_no_leftover();
    let (tls, server_key, _) = tls_upgrade(tcp, &p.host, Some(&p.fingerprint)).await?;
    let upgraded = ironrdp_tokio::mark_as_upgraded(should_upgrade, &mut connector);
    let mut framed = TokioFramed::new(tls);

    let result = tokio::time::timeout(
        Duration::from_secs(30),
        ironrdp_tokio::connect_finalize(upgraded, connector, &mut framed, &mut NoKerberos, ServerName::new(p.host.clone()), server_key, None),
    )
    .await
    .map_err(|_| Failure::Network("délai dépassé pendant l'ouverture de session RDP".into()))?;

    match result {
        Ok(r) => Ok((r, framed)),
        Err(e) => {
            let msg = e.report().to_string();
            let low = msg.to_lowercase();
            if low.contains("credssp") || low.contains("logon") || low.contains("ntlm") || low.contains("authent") || low.contains("password") {
                Err(Failure::Auth("authentification refusée par la cible (identifiant, mot de passe ou domaine incorrect)".into()))
            } else {
                Err(Failure::Protocol(format!("ouverture de session RDP : {msg}")))
            }
        }
    }
}

async fn probe(host: &str, port: u16) -> Result<String, Failure> {
    let tcp = tcp_connect(host, port).await?;
    let client_addr = tcp.local_addr().map_err(|e| Failure::Network(e.to_string()))?;
    let mut framed = TokioFramed::new(tcp);
    let p = Params {
        host: host.into(),
        port,
        username: "gna-probe".into(),
        password: String::new(),
        domain: None,
        width: 800,
        height: 600,
        fingerprint: String::new(),
        layout: String::new(),
        title: String::new(),
        clipboard: false,
        drive_dir: None,
        drive_max_mb: 0,
    };
    let mut connector = ClientConnector::new(rdp_config(&p, keys::Layout::Us), client_addr);
    tokio::time::timeout(CONNECT_TIMEOUT, ironrdp_tokio::connect_begin(&mut framed, &mut connector))
        .await
        .map_err(|_| Failure::Network("pas de réponse RDP (est-ce bien un serveur RDP ?)".into()))?
        .map_err(|e| Failure::Protocol(format!("négociation RDP : {}", e.report())))?;
    let (_, _, fp) = tls_upgrade(framed.into_inner_no_leftover(), host, None).await?;
    Ok(fp)
}

// ---------------------------------------------------------------------------
// Session : RDP ⇄ VNC
// ---------------------------------------------------------------------------

async fn bridge() -> i32 {
    let mut stdin = BufReader::new(tokio::io::stdin());
    let mut line = String::new();
    if stdin.read_line(&mut line).await.is_err() {
        report(&Status::Error { kind: "protocol", message: "paramètres absents".into() });
        return 2;
    }
    let mut p: Params = match serde_json::from_str(&line) {
        Ok(p) => p,
        Err(e) => {
            report(&Status::Error { kind: "protocol", message: format!("paramètres invalides : {e}") });
            return 2;
        }
    };
    line.clear(); // le mot de passe ne reste pas en mémoire plus que nécessaire
    // Largeur multiple de 4 et taille raisonnable (exigences RDP).
    p.width = (p.width.clamp(640, 3840) / 4) * 4;
    p.height = p.height.clamp(480, 2160);
    let layout = keys::Layout::parse(&p.layout);

    let (clip_tx, clip_rx) = tokio::sync::mpsc::unbounded_channel();
    let (result, framed) = match connect(&p, layout, clip_tx).await {
        Ok(r) => r,
        Err(e) => {
            report(&e.status());
            return 1;
        }
    };
    p.password.clear();

    let (w, h) = (result.desktop_size.width, result.desktop_size.height);
    report(&Status::Ready { width: w, height: h });

    let st = rfb::new_state(w, h);
    st.0.lock().unwrap().clipboard_allowed = p.clipboard;
    let mut stdout = tokio::io::stdout();
    let title = if p.title.is_empty() { format!("{} (RDP)", p.host) } else { p.title.clone() };
    if rfb::handshake(&mut stdin, &mut stdout, &st, &title).await.is_err() {
        return 0;
    }

    // Lecture des messages VNC (clavier, souris, demandes de rafraîchissement).
    let (tx, rx) = mpsc::channel::<rfb::ClientEvent>(256);
    let st_r = st.clone();
    tokio::spawn(async move {
        loop {
            match rfb::read_client(&mut stdin, &st_r).await {
                Ok(Some(ev)) => {
                    if tx.send(ev).await.is_err() {
                        break;
                    }
                }
                Ok(None) => {}
                Err(_) => break, // Gná a fermé la session
            }
        }
    });
    // Envoi de l'écran.
    let st_w = st.clone();
    let writer = tokio::spawn(async move { rfb::writer_loop(&mut stdout, st_w).await });

    let code = run_session(result, framed, st, rx, clip_rx, layout).await;
    writer.abort();
    code
}

async fn run_session(
    result: ConnectionResult,
    mut framed: Framed,
    st: rfb::State,
    mut rx: mpsc::Receiver<rfb::ClientEvent>,
    mut clip_rx: mpsc::UnboundedReceiver<clip::Event>,
    layout: keys::Layout,
) -> i32 {
    let mut image = DecodedImage::new(
        ironrdp::graphics::image_processing::PixelFormat::RgbA32,
        result.desktop_size.width,
        result.desktop_size.height,
    );
    let activation = result.activation_factory;
    let mut stage: ActiveStage = ActiveStageBuilder {
        static_channels: result.static_channels,
        user_channel_id: result.user_channel_id,
        io_channel_id: result.io_channel_id,
        message_channel_id: result.message_channel_id,
        share_id: result.share_id,
        compression_type: result.compression_type,
        enable_server_pointer: result.enable_server_pointer,
        pointer_software_rendering: result.pointer_software_rendering,
    }
    .build();
    let mut db = Database::new();
    let mut kb = keys::Keyboard::new(layout);
    let mut buttons = 0u8;
    let debug = std::env::var_os("GNA_RDP_DEBUG").is_some();
    // Texte collé depuis le navigateur, proposé à la machine distante.
    let mut local_text: Option<String> = None;
    // Redimensionnement demandé par le navigateur, en attente de la réactivation.
    let mut resize_deadline: Option<tokio::time::Instant> = None;
    // Copier-coller direct : quand le navigateur envoie un nouveau texte juste avant
    // Ctrl+V, clavier et souris sont retenus jusqu'à ce que la machine distante ait
    // reçu ce texte, pour que le collage utilise bien le nouveau contenu.
    let mut held: Vec<rfb::ClientEvent> = Vec::new();
    let mut hold_until: Option<tokio::time::Instant> = None;

    loop {
        // « biased » : clavier et souris passent avant le décodage de l'image suivante.
        let outputs = tokio::select! {
            biased;
            ev = rx.recv() => {
                let Some(ev) = ev else {
                    // Fin côté Gná : fermeture propre de la session RDP.
                    let ops = kb.release_all();
                    let _ = send_ops(&mut stage, &mut image, &mut db, ops, &mut framed, &st).await;
                    if let Ok(out) = stage.graceful_shutdown() {
                        for o in out {
                            if let ActiveStageOutput::ResponseFrame(f) = o { let _ = framed.write_all(&f).await; }
                        }
                    }
                    return 0;
                };
                if hold_until.is_some() && matches!(ev, rfb::ClientEvent::Key { .. } | rfb::ClientEvent::Pointer { .. }) {
                    held.push(ev);
                    continue;
                }
                let ops = match ev {
                    rfb::ClientEvent::ClipboardIncoming => {
                        if stage.get_svc_processor::<CliprdrClient>().is_some() {
                            hold_until = Some(tokio::time::Instant::now() + Duration::from_millis(1500));
                        }
                        continue;
                    }
                    rfb::ClientEvent::Key { keysym, down } => {
                        let ops = kb.event(keysym, down);
                        if debug { eprintln!("clavier keysym={keysym:#x} appui={down} -> {ops:?}"); }
                        ops
                    }
                    rfb::ClientEvent::Pointer { x, y, mask } => {
                        let ops = pointer_ops(&mut buttons, x, y, mask);
                        if debug && ops.len() > 1 { eprintln!("souris {x},{y} masque={mask} -> {ops:?}"); }
                        ops
                    }
                    rfb::ClientEvent::Clipboard(text) => {
                        if debug { eprintln!("presse-papiers du navigateur : {} caractères", text.chars().count()); }
                        if text.len() <= clip::MAX_TEXT {
                            local_text = Some(text);
                            if clip_send(&mut stage, &mut framed, |c| c.initiate_copy(&[ironrdp::cliprdr::pdu::ClipboardFormat::new(clip::CF_UNICODETEXT)])).await.is_err() {
                                return 0;
                            }
                        }
                        continue;
                    }
                    rfb::ClientEvent::Resize { width, height } => {
                        let (cw, ch) = { let s = st.0.lock().unwrap(); (s.width, s.height) };
                        // Contraintes RDP : largeur paire (multiple de 4 par sécurité), 200 à 8192 pixels.
                        let w = (u32::from(width).clamp(640, 3840) / 4) * 4;
                        let h = u32::from(height).clamp(480, 2160);
                        if w == u32::from(cw) && h == u32::from(ch) {
                            st.0.lock().unwrap().resize_refused(true);
                            st.1.notify_one();
                            continue;
                        }
                        match stage.encode_resize(w, h, None, None) {
                            Some(Ok(frame)) => {
                                if debug { eprintln!("redimensionnement demandé : {w}x{h}"); }
                                if framed.write_all(&frame).await.is_err() { return 0; }
                                resize_deadline = Some(tokio::time::Instant::now() + Duration::from_secs(10));
                            }
                            _ => {
                                // Canal de redimensionnement indisponible sur cette cible.
                                st.0.lock().unwrap().resize_refused(false);
                                st.1.notify_one();
                            }
                        }
                        continue;
                    }
                };
                match send_ops(&mut stage, &mut image, &mut db, ops, &mut framed, &st).await {
                    Ok(()) => continue,
                    Err(_) => return 0,
                }
            }
            Some(ev) = clip_rx.recv() => {
                if debug { eprintln!("presse-papiers : {ev:?}"); }
                let r = match ev {
                    clip::Event::Ready | clip::Event::FormatListRequested => {
                        let formats: Vec<_> = local_text.iter().map(|_| ironrdp::cliprdr::pdu::ClipboardFormat::new(clip::CF_UNICODETEXT)).collect();
                        clip_send(&mut stage, &mut framed, |c| c.initiate_copy(&formats)).await
                    }
                    clip::Event::RemoteCopy(Some(format)) => clip_send(&mut stage, &mut framed, |c| c.initiate_paste(format)).await,
                    clip::Event::RemoteCopy(None) => Ok(()),
                    clip::Event::DataRequested(format) => {
                        use ironrdp::cliprdr::pdu::FormatDataResponse;
                        let resp = match &local_text {
                            Some(t) if format == clip::CF_UNICODETEXT => FormatDataResponse::new_unicode_string(t),
                            Some(t) if format == clip::CF_TEXT => FormatDataResponse::new_string(t),
                            _ => FormatDataResponse::new_error(),
                        };
                        clip_send(&mut stage, &mut framed, |c| c.submit_format_data(resp)).await
                    }
                    clip::Event::Data(Some(text)) => {
                        st.0.lock().unwrap().push_clipboard(&text);
                        st.1.notify_one();
                        Ok(())
                    }
                    clip::Event::Data(None) => Ok(()),
                    clip::Event::FormatListAck => {
                        if hold_until.take().is_some() {
                            if release_held(&mut held, &mut stage, &mut image, &mut db, &mut kb, &mut buttons, &mut framed, &st).await.is_err() {
                                return 0;
                            }
                        }
                        Ok(())
                    }
                };
                if r.is_err() { return 0; }
                continue;
            }
            _ = sleep_until(hold_until) => {
                // Pas de confirmation de la machine distante : on n'attend pas plus.
                hold_until = None;
                if release_held(&mut held, &mut stage, &mut image, &mut db, &mut kb, &mut buttons, &mut framed, &st).await.is_err() {
                    return 0;
                }
                continue;
            }
            _ = sleep_until(resize_deadline) => {
                // La cible n'a pas appliqué la nouvelle taille : on débloque le navigateur.
                resize_deadline = None;
                st.0.lock().unwrap().resize_refused(false);
                st.1.notify_one();
                continue;
            }
            frame = framed.read_pdu() => match frame {
                // xrdp envoie un « Deactivate All » sans identifiant de partage, que
                // IronRDP refuse : on le reconnaît ici pour lancer la réactivation.
                Ok((ironrdp::pdu::Action::X224, ref payload)) if is_short_deactivate_all(payload) => vec![ActiveStageOutput::DeactivateAll],
                Ok((action, payload)) => match stage.process(&mut image, action, &payload) {
                    Ok(o) => o,
                    Err(e) => {
                        let text = format!("{e:?}");
                        if text.contains("disconnect provider ultimatum") {
                            eprintln!("session RDP fermée par la cible");
                            return 0;
                        }
                        // Détail complet (type de message en cause) pour le diagnostic.
                        eprintln!("erreur de session RDP : {text}");
                        if debug {
                            let head: String = payload.iter().take(96).map(|b| format!("{b:02x}")).collect();
                            eprintln!("détail : action={action:?} | taille={} | début={head}", payload.len());
                        }
                        return 1;
                    }
                },
                Err(_) => return 0, // connexion fermée par la cible
            },
        };
        match handle_outputs(outputs, &mut framed, &image, &st).await {
            Ok(Flow::Continue) => {}
            Ok(Flow::Reactivate) => {
                // Séquence de désactivation-réactivation (changement de taille, retour de
                // l'écran de connexion…) : nouvelle activation sur la même session, les
                // canaux (presse-papiers, lecteur, redimensionnement) sont conservés.
                if debug { eprintln!("réactivation demandée par la cible"); }
                match reactivate(&activation, &mut framed).await {
                    Ok((w, h, share_id, server_pointer, sw_pointer, io_id, user_id)) => {
                        image = DecodedImage::new(ironrdp::graphics::image_processing::PixelFormat::RgbA32, w, h);
                        stage.set_fastpath_processor(
                            ironrdp::session::fast_path::ProcessorBuilder {
                                io_channel_id: io_id,
                                user_channel_id: user_id,
                                share_id,
                                enable_server_pointer: server_pointer,
                                pointer_software_rendering: sw_pointer,
                                bulk_decompressor: None,
                            }
                            .build(),
                        );
                        stage.set_share_id(share_id);
                        stage.set_enable_server_pointer(server_pointer);
                        let requested = resize_deadline.take().is_some();
                        st.0.lock().unwrap().resize(w, h, requested);
                        st.1.notify_one();
                    }
                    Err(e) => {
                        eprintln!("réactivation RDP impossible : {e}");
                        return 1;
                    }
                }
            }
            Err(()) => return 0,
        }
    }
}

/// Envoie le clavier et la souris retenus pendant la transmission du presse-papiers.
#[allow(clippy::too_many_arguments)]
async fn release_held(
    held: &mut Vec<rfb::ClientEvent>,
    stage: &mut ActiveStage,
    image: &mut DecodedImage,
    db: &mut Database,
    kb: &mut keys::Keyboard,
    buttons: &mut u8,
    framed: &mut Framed,
    st: &rfb::State,
) -> Result<(), ()> {
    for ev in std::mem::take(held) {
        let ops = match ev {
            rfb::ClientEvent::Key { keysym, down } => kb.event(keysym, down),
            rfb::ClientEvent::Pointer { x, y, mask } => pointer_ops(buttons, x, y, mask),
            _ => continue,
        };
        send_ops(stage, image, db, ops, framed, st).await?;
    }
    Ok(())
}

/// Message « Deactivate All » réduit à son en-tête (6 octets), tel qu'envoyé par xrdp.
fn is_short_deactivate_all(p: &[u8]) -> bool {
    // TPKT (4) + X.224 données (3) + MCS Send Data Indication (0x68)…
    if p.len() < 14 || p[4..7] != [0x02, 0xF0, 0x80] || p[7] != 0x68 {
        return false;
    }
    // … initiateur (2), canal (2), priorité (1), longueur PER (1 ou 2 octets).
    let mut i = 13;
    let len = if p[i] & 0x80 != 0 {
        i += 1;
        (usize::from(p[i - 1] & 0x7F) << 8) | usize::from(*p.get(i).unwrap_or(&0))
    } else {
        usize::from(p[i])
    };
    i += 1;
    let Some(sc) = p.get(i..i + len) else { return false };
    if sc.len() < 4 {
        return false;
    }
    let total = u16::from_le_bytes([sc[0], sc[1]]);
    let pdu_type = u16::from_le_bytes([sc[2], sc[3]]) & 0x0F;
    pdu_type == 0x6 && total < 10
}

async fn sleep_until(deadline: Option<tokio::time::Instant>) {
    match deadline {
        Some(d) => tokio::time::sleep_until(d).await,
        None => std::future::pending().await,
    }
}

/// Envoie des messages du canal presse-papiers.
async fn clip_send<F>(stage: &mut ActiveStage, framed: &mut Framed, f: F) -> Result<(), ()>
where
    F: FnOnce(&mut CliprdrClient) -> ironrdp::pdu::PduResult<ironrdp::cliprdr::CliprdrSvcMessages<ironrdp::cliprdr::Client>>,
{
    let Some(c) = stage.get_svc_processor_mut::<CliprdrClient>() else { return Ok(()) };
    let msgs = match f(c) {
        Ok(m) => m,
        Err(e) => {
            eprintln!("presse-papiers : {e}");
            return Ok(());
        }
    };
    match stage.process_svc_processor_messages(msgs) {
        Ok(frame) => framed.write_all(&frame).await.map_err(|_| ()),
        Err(e) => {
            eprintln!("presse-papiers : {e}");
            Ok(())
        }
    }
}

enum Flow {
    Continue,
    Reactivate,
}

type Activation = (u16, u16, u32, bool, bool, u16, u16);

async fn reactivate(factory: &connector::connection_activation::ConnectionActivationFactory, framed: &mut Framed) -> Result<Activation, String> {
    use connector::connection_activation::ConnectionActivationState;
    let mut seq = factory.create();
    let mut buf = ironrdp::core::WriteBuf::new();
    loop {
        let written = ironrdp_tokio::single_sequence_step_read(framed, &mut seq, &mut buf)
            .await
            .map_err(|e| e.to_string())?;
        if written.size().is_some() {
            framed.write_all(buf.filled()).await.map_err(|e| e.to_string())?;
        }
        if let ConnectionActivationState::Finalized { desktop_size, share_id, enable_server_pointer, pointer_software_rendering } =
            seq.connection_activation_state()
        {
            return Ok((
                desktop_size.width,
                desktop_size.height,
                share_id,
                enable_server_pointer,
                pointer_software_rendering,
                seq.io_channel_id(),
                seq.user_channel_id(),
            ));
        }
    }
}

/// Applique des opérations clavier/souris et envoie les événements RDP correspondants.
async fn send_ops(
    stage: &mut ActiveStage,
    image: &mut DecodedImage,
    db: &mut Database,
    ops: Vec<Operation>,
    framed: &mut Framed,
    st: &rfb::State,
) -> Result<(), ()> {
    if ops.is_empty() {
        return Ok(());
    }
    let events = db.apply(ops);
    let out = stage.process_fastpath_input(image, &events).map_err(|_| ())?;
    handle_outputs(out, framed, image, st).await.map(|_| ())
}

async fn handle_outputs(outputs: Vec<ActiveStageOutput>, framed: &mut Framed, image: &DecodedImage, st: &rfb::State) -> Result<Flow, ()> {
    let mut drawn = false;
    let mut flow = Flow::Continue;
    for out in outputs {
        match out {
            ActiveStageOutput::ResponseFrame(f) => framed.write_all(&f).await.map_err(|_| ())?,
            ActiveStageOutput::GraphicsUpdate(r) => {
                let rect = rfb::Rect { x: r.left, y: r.top, w: r.right - r.left + 1, h: r.bottom - r.top + 1 };
                st.0.lock().unwrap().update(image.data(), image.stride(), rect);
                drawn = true;
            }
            ActiveStageOutput::Terminate(reason) => {
                eprintln!("session RDP terminée par la cible : {reason:?}");
                return Err(());
            }
            ActiveStageOutput::DeactivateAll => flow = Flow::Reactivate,
            _ => {}
        }
    }
    if drawn {
        st.1.notify_one();
    }
    Ok(flow)
}

/// Souris VNC (position + masque de boutons) → opérations RDP.
fn pointer_ops(prev: &mut u8, x: u16, y: u16, mask: u8) -> Vec<Operation> {
    let mut ops = vec![Operation::MouseMove(MousePosition { x, y })];
    for (bit, button) in [(1u8, MouseButton::Left), (2, MouseButton::Middle), (4, MouseButton::Right)] {
        let (was, is) = (*prev & bit != 0, mask & bit != 0);
        if is && !was {
            ops.push(Operation::MouseButtonPressed(button));
        } else if was && !is {
            ops.push(Operation::MouseButtonReleased(button));
        }
    }
    // Molette : boutons 4 (haut) et 5 (bas), pris à l'appui.
    if mask & 8 != 0 && *prev & 8 == 0 {
        ops.push(Operation::WheelRotations(WheelRotations { is_vertical: true, rotation_units: 120 }));
    }
    if mask & 16 != 0 && *prev & 16 == 0 {
        ops.push(Operation::WheelRotations(WheelRotations { is_vertical: true, rotation_units: -120 }));
    }
    *prev = mask;
    ops
}

#[cfg(test)]
mod tests {
    #[test]
    fn deactivate_all_court_xrdp() {
        let p = [0x03, 0x00, 0x00, 0x14, 0x02, 0xf0, 0x80, 0x68, 0x00, 0x06, 0x03, 0xeb, 0x70, 0x06, 0x06, 0x00, 0x16, 0x00, 0xef, 0x03];
        assert!(super::is_short_deactivate_all(&p));
        let mut other = p;
        other[16] = 0x17; // autre type de message
        assert!(!super::is_short_deactivate_all(&other));
    }
}
