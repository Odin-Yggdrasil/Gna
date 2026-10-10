//! Relais local de l'application Gná.
//!
//! Les moteurs web intégrés aux applications (WebView2, WebKitGTK, WebView Android)
//! refusent les certificats auto-signés. L'application démarre donc ce relais sur
//! 127.0.0.1 : la fenêtre charge l'interface de Gná en local, et le relais transmet
//! chaque requête (HTTP et WebSocket) au serveur en TLS.
//!
//! Le certificat du serveur est **épinglé** : seule l'empreinte SHA-256 validée par
//! l'utilisateur au premier lancement est acceptée. Un autre certificat (même signé
//! par une autorité reconnue) est refusé, ce qui protège contre l'interception.
//!
//! Le relais réécrit au passage ce qui dépend de l'adresse :
//!   - `Host` et `Origin` → l'adresse réelle du serveur (contrôles anti-CSRF de Gná) ;
//!   - cookie `__Host-gna_session` ⇄ `gna_session` (le préfixe `__Host-` et l'attribut
//!     `Secure` exigent du HTTPS, alors que la fenêtre parle en HTTP local) ;
//!   - en-tête CSP : autorisation explicite de `ws://127.0.0.1:<port>` ;
//!   - suppression de HSTS (sans objet en local).

use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use bytes::Bytes;
use http::header::{self, HeaderValue};
use http::{Request, Response, StatusCode};
use http_body_util::{combinators::BoxBody, BodyExt, Empty, Full};
use hyper::body::Incoming;
use hyper_util::rt::TokioIo;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, SignatureScheme};
use sha2::{Digest, Sha256};
use tokio::net::{TcpListener, TcpStream};
use tokio_rustls::TlsConnector;

type Body = BoxBody<Bytes, hyper::Error>;

/// Chemin spécial servi par le relais : retour à l'écran de réglages de l'application.
pub const SETTINGS_PATH: &str = "/__gna_app/settings";

const COOKIE_REMOTE: &str = "__Host-gna_session";
const COOKIE_LOCAL: &str = "gna_session";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

// ---------------------------------------------------------------------------
// Adresse du serveur
// ---------------------------------------------------------------------------

/// Serveur Gná visé.
#[derive(Clone, Debug, PartialEq)]
pub struct Upstream {
    pub host: String,
    pub port: u16,
}

impl Upstream {
    /// Accepte « 10.0.1.5 », « 10.0.1.5:8443 », « https://gna.lan:8443/ »…
    /// Sans port, 8443 (port par défaut de Gná) est utilisé.
    pub fn parse(input: &str) -> Result<Self, String> {
        let s = input.trim();
        if s.is_empty() {
            return Err("adresse vide".into());
        }
        if s.starts_with("http://") {
            return Err("Gná doit être joint en HTTPS (https://…)".into());
        }
        let with_scheme = if s.starts_with("https://") { s.to_string() } else { format!("https://{s}") };
        let u = url::Url::parse(&with_scheme).map_err(|_| "adresse invalide".to_string())?;
        let host = u.host_str().ok_or("adresse invalide")?.trim_matches(['[', ']']).to_string();
        let port = match u.port() {
            Some(p) => p,
            None if s.contains(":443") => 443,
            None => 8443,
        };
        Ok(Upstream { host, port })
    }

    /// Valeur des en-têtes Host / Origin attendue par le serveur.
    pub fn authority(&self) -> String {
        let h = if self.host.contains(':') { format!("[{}]", self.host) } else { self.host.clone() };
        if self.port == 443 { h } else { format!("{h}:{}", self.port) }
    }

    /// Adresse lisible, pour l'affichage (« https://10.0.1.5:8443 »).
    pub fn display(&self) -> String {
        format!("https://{}", self.authority())
    }
}

// ---------------------------------------------------------------------------
// Empreintes et vérification TLS
// ---------------------------------------------------------------------------

/// Empreinte SHA-256 au format « AA:BB:… » (identique à `gna tls info`).
pub fn fingerprint_hex(der: &[u8]) -> String {
    Sha256::digest(der).iter().map(|b| format!("{b:02X}")).collect::<Vec<_>>().join(":")
}

fn normalize_fp(fp: &str) -> String {
    fp.chars().filter(|c| c.is_ascii_hexdigit()).collect::<String>().to_ascii_uppercase()
}

/// Vérificateur TLS : le certificat présenté doit avoir exactement l'empreinte épinglée.
/// (Ou, en mode découverte, n'importe quel certificat, dont l'empreinte est relevée.)
/// Les signatures de la poignée de main restent vérifiées : le serveur doit posséder
/// la clé privée du certificat.
#[derive(Debug)]
struct PinVerifier {
    pinned: Option<String>, // empreinte normalisée ; None = découverte
    seen: Mutex<Option<String>>,
    provider: Arc<rustls::crypto::CryptoProvider>,
}

impl ServerCertVerifier for PinVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let fp = fingerprint_hex(end_entity.as_ref());
        *self.seen.lock().unwrap() = Some(fp.clone());
        match &self.pinned {
            None => Ok(ServerCertVerified::assertion()),
            Some(p) if *p == normalize_fp(&fp) => Ok(ServerCertVerified::assertion()),
            Some(_) => Err(rustls::Error::General("CERT_MISMATCH".into())),
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(message, cert, dss, &self.provider.signature_verification_algorithms)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(message, cert, dss, &self.provider.signature_verification_algorithms)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider.signature_verification_algorithms.supported_schemes()
    }
}

fn tls_connector(pinned: Option<&str>) -> (TlsConnector, Arc<PinVerifier>) {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = Arc::new(PinVerifier {
        pinned: pinned.map(normalize_fp),
        seen: Mutex::new(None),
        provider: provider.clone(),
    });
    let config = rustls::ClientConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .expect("versions TLS")
        .dangerous()
        .with_custom_certificate_verifier(verifier.clone())
        .with_no_client_auth();
    (TlsConnector::from(Arc::new(config)), verifier)
}

/// Erreurs de connexion au serveur, avec un message en français.
#[derive(Debug, Clone, PartialEq)]
pub enum ConnectError {
    /// Le certificat présenté n'est pas celui épinglé (empreinte reçue).
    CertMismatch(String),
    Unreachable(String),
    Tls(String),
}

impl std::fmt::Display for ConnectError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConnectError::CertMismatch(fp) => write!(f, "le certificat du serveur a changé (nouvelle empreinte : {fp})"),
            ConnectError::Unreachable(e) => write!(f, "serveur injoignable : {e}"),
            ConnectError::Tls(e) => write!(f, "échec de la connexion sécurisée : {e}"),
        }
    }
}

async fn connect_tls(
    up: &Upstream,
    pinned: Option<&str>,
) -> Result<(tokio_rustls::client::TlsStream<TcpStream>, String), ConnectError> {
    let tcp = tokio::time::timeout(CONNECT_TIMEOUT, TcpStream::connect((up.host.as_str(), up.port)))
        .await
        .map_err(|_| ConnectError::Unreachable("délai dépassé".into()))?
        .map_err(|e| ConnectError::Unreachable(e.to_string()))?;
    tcp.set_nodelay(true).ok();
    let (connector, verifier) = tls_connector(pinned);
    let name = ServerName::try_from(up.host.clone()).map_err(|_| ConnectError::Tls("nom de serveur invalide".into()))?;
    match tokio::time::timeout(CONNECT_TIMEOUT, connector.connect(name, tcp)).await {
        Err(_) => Err(ConnectError::Unreachable("délai dépassé pendant la poignée de main TLS".into())),
        Ok(Ok(s)) => {
            let fp = verifier.seen.lock().unwrap().clone().unwrap_or_default();
            Ok((s, fp))
        }
        Ok(Err(e)) => {
            let seen = verifier.seen.lock().unwrap().clone().unwrap_or_default();
            if e.to_string().contains("CERT_MISMATCH") {
                Err(ConnectError::CertMismatch(seen))
            } else {
                Err(ConnectError::Tls(e.to_string()))
            }
        }
    }
}

/// Relève l'empreinte du certificat présenté par le serveur (premier lancement).
pub async fn probe(up: &Upstream) -> Result<String, ConnectError> {
    connect_tls(up, None).await.map(|(_, fp)| fp)
}

// ---------------------------------------------------------------------------
// Relais HTTP / WebSocket
// ---------------------------------------------------------------------------

/// État partagé par les connexions du relais.
struct Relay {
    up: Upstream,
    pin: String,
    local: String, // « 127.0.0.1:port »
    on_settings: Option<Arc<dyn Fn() + Send + Sync>>,
    last_error: Mutex<Option<ConnectError>>,
}

/// Relais démarré.
pub struct Handle {
    pub addr: SocketAddr,
    relay: Arc<Relay>,
    task: tokio::task::JoinHandle<()>,
}

impl Handle {
    /// Adresse à charger dans la fenêtre (« http://127.0.0.1:port/ »).
    pub fn url(&self) -> String {
        format!("http://{}/", self.addr)
    }
    /// Dernière erreur de connexion au serveur (pour l'afficher à l'utilisateur).
    pub fn last_error(&self) -> Option<ConnectError> {
        self.relay.last_error.lock().unwrap().clone()
    }
    pub fn stop(self) {
        self.task.abort();
    }
}

/// Démarre le relais sur 127.0.0.1. Le port `preferred` est essayé d'abord (0 = aucun) :
/// garder le même port d'un lancement à l'autre conserve les réglages enregistrés par
/// la page (thème, dossiers repliés…), liés à l'adresse. S'il est pris, le système en
/// choisit un autre. `on_settings` est appelé quand la page demande le retour aux réglages.
pub async fn start(
    up: Upstream,
    pinned_fingerprint: &str,
    preferred: u16,
    on_settings: Option<Arc<dyn Fn() + Send + Sync>>,
) -> std::io::Result<Handle> {
    let listener = match TcpListener::bind(("127.0.0.1", preferred)).await {
        Ok(l) => l,
        Err(_) if preferred != 0 => TcpListener::bind(("127.0.0.1", 0)).await?,
        Err(e) => return Err(e),
    };
    let addr = listener.local_addr()?;
    let relay = Arc::new(Relay {
        up,
        pin: pinned_fingerprint.to_string(),
        local: addr.to_string(),
        on_settings,
        last_error: Mutex::new(None),
    });
    let r = relay.clone();
    let task = tokio::spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else { continue };
            stream.set_nodelay(true).ok();
            let r = r.clone();
            tokio::spawn(async move {
                let svc = hyper::service::service_fn(move |req| {
                    let r = r.clone();
                    async move { Ok::<_, Infallible>(r.handle(req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(stream), svc)
                    .with_upgrades()
                    .await;
            });
        }
    });
    Ok(Handle { addr, relay, task })
}

impl Relay {
    async fn handle(self: Arc<Self>, mut req: Request<Incoming>) -> Response<Body> {
        // Refuser toute requête qui ne vise pas exactement l'adresse du relais :
        // protège contre le « DNS rebinding » depuis un site web malveillant.
        let host_ok = req.headers().get(header::HOST).and_then(|h| h.to_str().ok()) == Some(self.local.as_str());
        if !host_ok {
            return text(StatusCode::FORBIDDEN, "Hôte refusé par le relais Gná.");
        }
        if req.uri().path() == SETTINGS_PATH {
            if let Some(cb) = &self.on_settings {
                cb();
            }
            return text(StatusCode::OK, "Retour aux réglages…");
        }

        let is_upgrade = req.headers().contains_key(header::UPGRADE);
        let client_upgrade = if is_upgrade { Some(hyper::upgrade::on(&mut req)) } else { None };

        let (parts, body) = req.into_parts();
        let mut fwd = Request::builder().method(parts.method.clone()).uri(
            parts.uri.path_and_query().map(|p| p.as_str()).unwrap_or("/").to_string(),
        );
        let authority = self.up.authority();
        for (name, value) in parts.headers.iter() {
            match *name {
                header::HOST | header::REFERER => continue,
                header::ORIGIN => {
                    fwd = fwd.header(header::ORIGIN, format!("https://{authority}"));
                }
                header::COOKIE => {
                    let v = value.to_str().unwrap_or("");
                    fwd = fwd.header(header::COOKIE, rename_request_cookie(v));
                }
                _ => {
                    fwd = fwd.header(name, value);
                }
            }
        }
        fwd = fwd.header(header::HOST, authority.as_str());
        let fwd = match fwd.body(body) {
            Ok(f) => f,
            Err(_) => return text(StatusCode::BAD_REQUEST, "Requête invalide."),
        };

        // Une connexion TLS par requête vers le serveur : simple et robuste (le serveur
        // est sur le réseau local, la poignée de main TLS 1.3 est rapide).
        let (tls, _) = match connect_tls(&self.up, Some(&self.pin)).await {
            Ok(c) => c,
            Err(e) => {
                *self.last_error.lock().unwrap() = Some(e.clone());
                return error_page(&e);
            }
        };
        *self.last_error.lock().unwrap() = None;

        let (mut sender, conn) = match hyper::client::conn::http1::handshake(TokioIo::new(tls)).await {
            Ok(x) => x,
            Err(e) => return error_page(&ConnectError::Tls(e.to_string())),
        };
        tokio::spawn(async move {
            let _ = conn.with_upgrades().await;
        });

        let mut resp = match sender.send_request(fwd).await {
            Ok(r) => r,
            Err(e) => return error_page(&ConnectError::Unreachable(e.to_string())),
        };

        // WebSocket (terminal SSH, bureau VNC) : on relie les deux connexions telles quelles.
        if resp.status() == StatusCode::SWITCHING_PROTOCOLS {
            if let Some(client_upgrade) = client_upgrade {
                let server_upgrade = hyper::upgrade::on(&mut resp);
                tokio::spawn(async move {
                    if let (Ok(c), Ok(s)) = (client_upgrade.await, server_upgrade.await) {
                        let mut c = TokioIo::new(c);
                        let mut s = TokioIo::new(s);
                        let _ = tokio::io::copy_bidirectional(&mut c, &mut s).await;
                    }
                });
            }
        }

        let (mut parts, body) = resp.into_parts();
        self.rewrite_response_headers(&mut parts.headers);
        Response::from_parts(parts, body.boxed())
    }

    fn rewrite_response_headers(&self, h: &mut http::HeaderMap) {
        h.remove(header::STRICT_TRANSPORT_SECURITY);
        let cookies: Vec<HeaderValue> = h
            .get_all(header::SET_COOKIE)
            .iter()
            .filter_map(|v| v.to_str().ok())
            .filter_map(|v| HeaderValue::from_str(&rewrite_set_cookie(v)).ok())
            .collect();
        h.remove(header::SET_COOKIE);
        for c in cookies {
            h.append(header::SET_COOKIE, c);
        }
        if let Some(csp) = h.get(header::CONTENT_SECURITY_POLICY).and_then(|v| v.to_str().ok()) {
            let ws = format!("connect-src 'self' ws://{}", self.local);
            if let Ok(v) = HeaderValue::from_str(&csp.replacen("connect-src 'self'", &ws, 1)) {
                h.insert(header::CONTENT_SECURITY_POLICY, v);
            }
        }
    }
}

/// `gna_session=…` (fenêtre locale) → `__Host-gna_session=…` (serveur).
fn rename_request_cookie(v: &str) -> String {
    v.split(';')
        .map(|c| {
            let c = c.trim();
            match c.strip_prefix(&format!("{COOKIE_LOCAL}=")) {
                Some(val) => format!("{COOKIE_REMOTE}={val}"),
                None => c.to_string(),
            }
        })
        .collect::<Vec<_>>()
        .join("; ")
}

/// `__Host-gna_session=…; Secure; …` (serveur) → `gna_session=…; …` (fenêtre locale).
fn rewrite_set_cookie(v: &str) -> String {
    v.split(';')
        .map(str::trim)
        .filter(|a| !a.eq_ignore_ascii_case("secure"))
        .map(|a| match a.strip_prefix(&format!("{COOKIE_REMOTE}=")) {
            Some(val) => format!("{COOKIE_LOCAL}={val}"),
            None => a.to_string(),
        })
        .collect::<Vec<_>>()
        .join("; ")
}

fn text(status: StatusCode, msg: &str) -> Response<Body> {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .body(Full::new(Bytes::from(msg.to_string())).map_err(|e| match e {}).boxed())
        .unwrap()
}

fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;").replace('"', "&quot;")
}

/// Page affichée dans la fenêtre quand le serveur est injoignable ou que son
/// certificat a changé, avec un bouton de retour aux réglages.
fn error_page(e: &ConnectError) -> Response<Body> {
    let (title, detail) = match e {
        ConnectError::CertMismatch(fp) => (
            "Certificat du serveur différent",
            format!(
                "Le serveur présente un certificat qui n'est pas celui que vous avez validé. \
                 Il peut s'agir d'un certificat régénéré (changement d'adresse IP, réinstallation) \
                 ou d'une tentative d'interception. La connexion a été bloquée.<br><br>\
                 Nouvelle empreinte : <code>{}</code><br><br>\
                 Vérifiez-la sur le serveur avec <code>sudo -u gna gna tls info</code> avant de l'accepter \
                 depuis les réglages.",
                html_escape(fp)
            ),
        ),
        other => ("Serveur Gná injoignable", html_escape(&other.to_string())),
    };
    let page = format!(
        r#"<!doctype html><html lang="fr"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Gná</title>
<style>
body{{margin:0;min-height:100vh;display:grid;place-items:center;background:#0e1116;color:#e6e9ef;font-family:system-ui,sans-serif;padding:16px;box-sizing:border-box}}
.c{{max-width:520px;background:#161b22;border:1px solid #2a313c;border-radius:10px;padding:28px}}
h1{{font-size:19px;margin:0 0 12px;color:#ff7b72}} p{{line-height:1.55;margin:0 0 18px}}
code{{font-size:12px;color:#5fb3a1;word-break:break-all}}
a{{display:inline-block;margin-right:10px;padding:10px 14px;border-radius:8px;text-decoration:none;font-weight:600}}
.p{{background:#5fb3a1;color:#0b1714}} .s{{border:1px solid #2a313c;color:#e6e9ef}}
</style>
<div class="c"><h1>{title}</h1><p>{detail}</p>
<a class="p" href="{SETTINGS_PATH}">Réglages</a><a class="s" href="/">Réessayer</a></div></html>"#
    );
    Response::builder()
        .status(StatusCode::BAD_GATEWAY)
        .header(header::CONTENT_TYPE, "text/html; charset=utf-8")
        .header(header::CACHE_CONTROL, "no-store")
        .body(Full::new(Bytes::from(page)).map_err(|e| match e {}).boxed())
        .unwrap()
}

#[allow(dead_code)]
fn empty() -> Body {
    Empty::<Bytes>::new().map_err(|e| match e {}).boxed()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_addresses() {
        assert_eq!(Upstream::parse("10.0.1.5").unwrap(), Upstream { host: "10.0.1.5".into(), port: 8443 });
        assert_eq!(Upstream::parse("https://gna.lan:9000/").unwrap(), Upstream { host: "gna.lan".into(), port: 9000 });
        assert_eq!(Upstream::parse("gna.lan:443").unwrap().authority(), "gna.lan");
        assert!(Upstream::parse("http://10.0.1.5").is_err());
        assert_eq!(Upstream::parse("[::1]:8443").unwrap().authority(), "[::1]:8443");
    }

    #[test]
    fn cookies() {
        assert_eq!(rename_request_cookie("a=1; gna_session=XYZ"), "a=1; __Host-gna_session=XYZ");
        assert_eq!(
            rewrite_set_cookie("__Host-gna_session=XYZ; Path=/; Max-Age=300; HttpOnly; Secure; SameSite=Strict"),
            "gna_session=XYZ; Path=/; Max-Age=300; HttpOnly; SameSite=Strict"
        );
    }

    #[test]
    fn fingerprint_normalization() {
        assert_eq!(normalize_fp("aa:bb:0c"), "AABB0C");
        assert_eq!(normalize_fp("AA BB 0C"), "AABB0C");
    }
}
