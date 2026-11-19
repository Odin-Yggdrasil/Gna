//! Côté VNC de la passerelle : un petit serveur RFB 3.8 qui expose l'écran RDP.
//!
//! L'écran est tenu en RGBA. Les zones modifiées sont envoyées à la demande du client
//! (le protocole RFB fonctionne « à la demande »), compressées en Zlib si le client
//! l'accepte (noVNC oui), sinon brutes.

use std::io;
use std::sync::{Arc, Mutex};

use flate2::{Compress, Compression, FlushCompress};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::Notify;

pub const ENC_RAW: i32 = 0;
pub const ENC_ZLIB: i32 = 6;
pub const ENC_DESKTOP_SIZE: i32 = -223;
pub const ENC_EXT_DESKTOP_SIZE: i32 = -308;
pub const ENC_EXT_CLIPBOARD: i32 = 0xC0A1_E5CEu32 as i32;

// Presse-papiers étendu (UTF-8) : actions et format texte.
const CLIP_CAPS: u32 = 1 << 24;
const CLIP_REQUEST: u32 = 1 << 25;
const CLIP_PEEK: u32 = 1 << 26;
const CLIP_NOTIFY: u32 = 1 << 27;
const CLIP_PROVIDE: u32 = 1 << 28;
const CLIP_TEXT: u32 = 1;
/// Taille maximale d'un message de presse-papiers accepté du client.
const MAX_CUT: usize = 4 * 1024 * 1024;

/// Rectangle (coordonnées en pixels, bornes exclues à droite et en bas).
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Rect {
    pub x: u16,
    pub y: u16,
    pub w: u16,
    pub h: u16,
}

impl Rect {
    fn union(self, o: Rect) -> Rect {
        let x = self.x.min(o.x);
        let y = self.y.min(o.y);
        let r = (self.x + self.w).max(o.x + o.w);
        let b = (self.y + self.h).max(o.y + o.h);
        Rect { x, y, w: r - x, h: b - y }
    }
}

/// Format de pixel demandé par le client (seul le 32 bits « vraies couleurs » est géré).
#[derive(Clone, Copy, Debug)]
pub struct PixelFormat {
    pub big_endian: bool,
    pub red_shift: u8,
    pub green_shift: u8,
    pub blue_shift: u8,
}

impl Default for PixelFormat {
    fn default() -> Self {
        // Format annoncé dans ServerInit : octets R, G, B, X (= celui de noVNC).
        PixelFormat { big_endian: false, red_shift: 0, green_shift: 8, blue_shift: 16 }
    }
}

/// État partagé entre la session RDP (qui dessine) et l'envoi VNC.
pub struct Shared {
    pub width: u16,
    pub height: u16,
    pub rgba: Vec<u8>,
    dirty: Vec<Rect>,
    resized: bool,
    request: Option<bool>, // Some(incrémentale ?) : le client attend une mise à jour
    format: PixelFormat,
    zlib: bool,
    desktop_size: bool,
    ext_desktop: bool,
    /// ExtendedDesktopSize à envoyer : (raison, état). Raison 1 = réponse à la demande du client.
    ext_pending: Option<(u16, u16)>,
    ext_clipboard: bool,
    /// Presse-papiers autorisé par Gná pour cette session.
    pub clipboard_allowed: bool,
    /// Messages hors image (presse-papiers) en attente d'envoi.
    outbox: Vec<Vec<u8>>,
}

pub type State = Arc<(Mutex<Shared>, Notify)>;

pub fn new_state(width: u16, height: u16) -> State {
    Arc::new((
        Mutex::new(Shared {
            width,
            height,
            rgba: vec![0; width as usize * height as usize * 4],
            dirty: Vec::new(),
            resized: false,
            request: None,
            format: PixelFormat::default(),
            zlib: false,
            desktop_size: false,
            ext_desktop: false,
            ext_pending: None,
            ext_clipboard: false,
            clipboard_allowed: false,
            outbox: Vec::new(),
        }),
        Notify::new(),
    ))
}

impl Shared {
    /// Copie une zone de l'image RDP (RGBA, `stride` octets par ligne) et la marque modifiée.
    pub fn update(&mut self, src: &[u8], stride: usize, r: Rect) {
        let r = self.clip(r);
        if r.w == 0 || r.h == 0 {
            return;
        }
        let w = self.width as usize;
        for row in r.y as usize..(r.y + r.h) as usize {
            let s = row * stride + r.x as usize * 4;
            let d = (row * w + r.x as usize) * 4;
            let n = r.w as usize * 4;
            if s + n <= src.len() {
                self.rgba[d..d + n].copy_from_slice(&src[s..s + n]);
            }
        }
        self.mark(r);
    }

    fn clip(&self, r: Rect) -> Rect {
        let x = r.x.min(self.width);
        let y = r.y.min(self.height);
        let w = r.w.min(self.width - x);
        let h = r.h.min(self.height - y);
        Rect { x, y, w, h }
    }

    fn mark(&mut self, r: Rect) {
        // Fusion simple : au-delà de 64 zones, on envoie leur enveloppe.
        self.dirty.push(r);
        if self.dirty.len() > 64 {
            let all = self.dirty.drain(..).reduce(Rect::union).unwrap();
            self.dirty.push(all);
        }
    }

    /// Nouvelle taille d'écran (réactivation de la session RDP). `requested` : en réponse
    /// à une demande de redimensionnement du client.
    pub fn resize(&mut self, width: u16, height: u16, requested: bool) {
        self.width = width;
        self.height = height;
        self.rgba = vec![0; width as usize * height as usize * 4];
        self.dirty.clear();
        self.resized = true;
        if self.ext_desktop {
            self.ext_pending = Some((if requested { 1 } else { 0 }, 0));
        }
    }

    /// Réponse à une demande de redimensionnement refusée ou sans effet.
    pub fn resize_refused(&mut self, unchanged: bool) {
        if self.ext_desktop {
            self.ext_pending = Some((1, if unchanged { 0 } else { 1 }));
        }
    }

    /// Texte copié sur la machine distante → presse-papiers du navigateur.
    pub fn push_clipboard(&mut self, text: &str) {
        if !self.clipboard_allowed {
            return;
        }
        let mut m = vec![3, 0, 0, 0];
        if self.ext_clipboard {
            let mut raw = Vec::with_capacity(text.len() + 5);
            let body = text.replace("\r\n", "\n").replace('\n', "\r\n");
            raw.extend_from_slice(&((body.len() + 1) as u32).to_be_bytes());
            raw.extend_from_slice(body.as_bytes());
            raw.push(0);
            let mut z = flate2::write::ZlibEncoder::new(Vec::new(), Compression::default());
            use std::io::Write as _;
            if z.write_all(&raw).is_err() {
                return;
            }
            let Ok(comp) = z.finish() else { return };
            m.extend_from_slice(&(-((comp.len() + 4) as i32)).to_be_bytes());
            m.extend_from_slice(&(CLIP_PROVIDE | CLIP_TEXT).to_be_bytes());
            m.extend_from_slice(&comp);
        } else {
            // Presse-papiers RFB classique : Latin-1.
            let latin: Vec<u8> = text.chars().map(|c| if (c as u32) < 256 { c as u8 } else { b'?' }).collect();
            m.extend_from_slice(&(latin.len() as i32).to_be_bytes());
            m.extend_from_slice(&latin);
        }
        self.outbox.push(m);
    }

    fn clipboard_caps(&mut self) {
        let mut m = vec![3, 0, 0, 0];
        m.extend_from_slice(&(-8i32).to_be_bytes());
        m.extend_from_slice(&(CLIP_CAPS | CLIP_REQUEST | CLIP_PEEK | CLIP_NOTIFY | CLIP_PROVIDE | CLIP_TEXT).to_be_bytes());
        m.extend_from_slice(&(MAX_CUT as u32).to_be_bytes());
        self.outbox.push(m);
    }

    fn clipboard_request(&mut self) {
        let mut m = vec![3, 0, 0, 0];
        m.extend_from_slice(&(-4i32).to_be_bytes());
        m.extend_from_slice(&(CLIP_REQUEST | CLIP_TEXT).to_be_bytes());
        self.outbox.push(m);
    }

    fn convert(&self, r: Rect, out: &mut Vec<u8>) {
        let f = self.format;
        let direct = !f.big_endian && f.red_shift == 0 && f.green_shift == 8 && f.blue_shift == 16;
        let w = self.width as usize;
        for row in r.y as usize..(r.y + r.h) as usize {
            let start = (row * w + r.x as usize) * 4;
            let line = &self.rgba[start..start + r.w as usize * 4];
            if direct {
                out.extend_from_slice(line);
            } else {
                for p in line.chunks_exact(4) {
                    let v = (p[0] as u32) << f.red_shift | (p[1] as u32) << f.green_shift | (p[2] as u32) << f.blue_shift;
                    out.extend_from_slice(&if f.big_endian { v.to_be_bytes() } else { v.to_le_bytes() });
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Poignée de main RFB (côté serveur)
// ---------------------------------------------------------------------------

pub async fn handshake<R, W>(r: &mut R, w: &mut W, st: &State, name: &str) -> io::Result<()>
where
    R: AsyncRead + Unpin,
    W: AsyncWrite + Unpin,
{
    w.write_all(b"RFB 003.008\n").await?;
    w.flush().await?;
    let mut ver = [0u8; 12];
    r.read_exact(&mut ver).await?;
    if &ver[..4] != b"RFB " {
        return Err(io::Error::new(io::ErrorKind::InvalidData, "client RFB invalide"));
    }
    w.write_all(&[1, 1]).await?; // un type de sécurité : « aucune » (l'accès est contrôlé par Gná)
    w.flush().await?;
    let mut choice = [0u8; 1];
    r.read_exact(&mut choice).await?;
    w.write_all(&0u32.to_be_bytes()).await?; // SecurityResult : OK
    w.flush().await?;
    r.read_exact(&mut choice).await?; // ClientInit (partage)

    let (width, height) = {
        let s = st.0.lock().unwrap();
        (s.width, s.height)
    };
    let mut init = Vec::with_capacity(64);
    init.extend_from_slice(&width.to_be_bytes());
    init.extend_from_slice(&height.to_be_bytes());
    // 32 bits par pixel, profondeur 24, little-endian, vraies couleurs, R/G/B sur 8 bits, décalages 0/8/16.
    init.extend_from_slice(&[32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0]);
    init.extend_from_slice(&(name.len() as u32).to_be_bytes());
    init.extend_from_slice(name.as_bytes());
    w.write_all(&init).await?;
    w.flush().await
}

// ---------------------------------------------------------------------------
// Messages du client
// ---------------------------------------------------------------------------

pub enum ClientEvent {
    Key { keysym: u32, down: bool },
    Pointer { x: u16, y: u16, mask: u8 },
    /// Texte collé depuis le navigateur.
    Clipboard(String),
    /// Le navigateur annonce un nouveau texte (il arrive juste après) : les touches
    /// suivantes (Ctrl+V) doivent attendre qu'il soit transmis à la machine distante.
    ClipboardIncoming,
    /// Le client demande une nouvelle taille de bureau.
    Resize { width: u16, height: u16 },
}

/// Lit le prochain message du client. Les demandes de rafraîchissement et les réglages
/// sont traités directement ; clavier et souris sont renvoyés à l'appelant.
pub async fn read_client<R: AsyncRead + Unpin>(r: &mut R, st: &State) -> io::Result<Option<ClientEvent>> {
    let t = r.read_u8().await?;
    match t {
        0 => {
            // SetPixelFormat
            let mut b = [0u8; 19];
            r.read_exact(&mut b).await?;
            let (bpp, true_color) = (b[3], b[6]);
            if bpp != 32 || true_color == 0 {
                return Err(io::Error::new(io::ErrorKind::Unsupported, "format de pixel non pris en charge (32 bits requis)"));
            }
            let mut s = st.0.lock().unwrap();
            s.format = PixelFormat { big_endian: b[5] != 0, red_shift: b[13], green_shift: b[14], blue_shift: b[15] };
            Ok(None)
        }
        2 => {
            // SetEncodings
            let mut b = [0u8; 3];
            r.read_exact(&mut b).await?;
            let n = u16::from_be_bytes([b[1], b[2]]) as usize;
            let mut encs = vec![0u8; n * 4];
            r.read_exact(&mut encs).await?;
            let list: Vec<i32> = encs.chunks_exact(4).map(|c| i32::from_be_bytes([c[0], c[1], c[2], c[3]])).collect();
            let mut s = st.0.lock().unwrap();
            s.zlib = list.contains(&ENC_ZLIB);
            s.desktop_size = list.contains(&ENC_DESKTOP_SIZE);
            let ext = list.contains(&ENC_EXT_DESKTOP_SIZE);
            if ext && !s.ext_desktop {
                // Annonce de la taille : le client sait alors qu'il peut demander un redimensionnement.
                s.ext_pending = Some((0, 0));
            }
            s.ext_desktop = ext;
            let clip = list.contains(&ENC_EXT_CLIPBOARD) && s.clipboard_allowed;
            if clip && !s.ext_clipboard {
                s.ext_clipboard = true;
                s.clipboard_caps();
            }
            drop(s);
            st.1.notify_one();
            Ok(None)
        }
        3 => {
            // FramebufferUpdateRequest
            let mut b = [0u8; 9];
            r.read_exact(&mut b).await?;
            let incremental = b[0] != 0;
            {
                let mut s = st.0.lock().unwrap();
                s.request = Some(match s.request {
                    Some(prev) => prev && incremental,
                    None => incremental,
                });
            }
            st.1.notify_one();
            Ok(None)
        }
        4 => {
            let mut b = [0u8; 7];
            r.read_exact(&mut b).await?;
            Ok(Some(ClientEvent::Key { down: b[0] != 0, keysym: u32::from_be_bytes([b[3], b[4], b[5], b[6]]) }))
        }
        5 => {
            let mut b = [0u8; 5];
            r.read_exact(&mut b).await?;
            Ok(Some(ClientEvent::Pointer {
                mask: b[0],
                x: u16::from_be_bytes([b[1], b[2]]),
                y: u16::from_be_bytes([b[3], b[4]]),
            }))
        }
        6 => {
            // ClientCutText : classique (Latin-1) ou étendu (UTF-8, compressé).
            let mut b = [0u8; 7];
            r.read_exact(&mut b).await?;
            let len = i32::from_be_bytes([b[3], b[4], b[5], b[6]]);
            let size = len.unsigned_abs() as usize;
            if size > MAX_CUT {
                tokio::io::copy(&mut r.take(size as u64), &mut tokio::io::sink()).await?;
                return Ok(None);
            }
            let mut data = vec![0u8; size];
            r.read_exact(&mut data).await?;
            if !st.0.lock().unwrap().clipboard_allowed {
                return Ok(None);
            }
            if len >= 0 {
                let text: String = data.iter().map(|&c| c as char).collect();
                return Ok(Some(ClientEvent::Clipboard(text)));
            }
            if data.len() < 4 {
                return Ok(None);
            }
            let flags = u32::from_be_bytes([data[0], data[1], data[2], data[3]]);
            let action = flags & 0xFF00_0000;
            if flags & CLIP_TEXT == 0 {
                return Ok(None);
            }
            if action == CLIP_NOTIFY {
                // Le navigateur a du texte : on le demande.
                st.0.lock().unwrap().clipboard_request();
                st.1.notify_one();
                return Ok(Some(ClientEvent::ClipboardIncoming));
            } else if action == CLIP_PROVIDE {
                return Ok(extended_text(&data[4..]).map(ClientEvent::Clipboard));
            }
            Ok(None)
        }
        251 => {
            // SetDesktopSize (ExtendedDesktopSize)
            let mut b = [0u8; 7];
            r.read_exact(&mut b).await?;
            let width = u16::from_be_bytes([b[1], b[2]]);
            let height = u16::from_be_bytes([b[3], b[4]]);
            let screens = b[5] as usize;
            let mut skip = vec![0u8; screens * 16];
            r.read_exact(&mut skip).await?;
            Ok(Some(ClientEvent::Resize { width, height }))
        }
        other => Err(io::Error::new(io::ErrorKind::InvalidData, format!("message RFB inconnu : {other}"))),
    }
}

/// Contenu d'un message « provide » du presse-papiers étendu : zlib([taille][texte UTF-8 + NUL]).
fn extended_text(zdata: &[u8]) -> Option<String> {
    use std::io::Read as _;
    let mut raw = Vec::new();
    // noVNC ne termine pas le flux (vidage simple, sans marqueur de fin) : on garde
    // tout ce qui a pu être décompressé, l'erreur de fin de flux est normale.
    let _ = flate2::read::ZlibDecoder::new(zdata).take(MAX_CUT as u64 + 8).read_to_end(&mut raw);
    if raw.len() < 4 {
        return None;
    }
    let n = (u32::from_be_bytes([raw[0], raw[1], raw[2], raw[3]]) as usize).min(raw.len() - 4);
    let text = String::from_utf8_lossy(&raw[4..4 + n]);
    Some(text.trim_end_matches('\0').replace("\r\n", "\n"))
}

// ---------------------------------------------------------------------------
// Envoi des mises à jour d'écran
// ---------------------------------------------------------------------------

/// Boucle d'envoi : dès que le client a demandé une mise à jour et que quelque chose a
/// changé, envoie les zones modifiées.
pub async fn writer_loop<W: AsyncWrite + Unpin>(w: &mut W, st: State) -> io::Result<()> {
    let mut z = Compress::new(Compression::fast(), true);
    let mut pixels = Vec::new();
    loop {
        st.1.notified().await;
        let msg = {
            let mut s = st.0.lock().unwrap();
            let mut out: Vec<u8> = s.outbox.drain(..).flatten().collect();
            if let Some(incremental) = s.request {
                let full = Rect { x: 0, y: 0, w: s.width, h: s.height };
                let mut rects: Vec<Rect> = if !incremental { vec![full] } else { std::mem::take(&mut s.dirty) };
                let resized = std::mem::take(&mut s.resized);
                if resized {
                    rects = vec![full];
                }
                let ext = s.ext_pending.take();
                let plain_size = resized && s.desktop_size && !s.ext_desktop;
                if rects.is_empty() && ext.is_none() {
                    // rien de neuf : on attend un dessin
                } else {
                    s.request = None;
                    s.dirty.clear();
                    let count = rects.len() + usize::from(plain_size) + usize::from(ext.is_some());
                    out.extend_from_slice(&[0, 0]);
                    out.extend_from_slice(&(count as u16).to_be_bytes());
                    if let Some((reason, status)) = ext {
                        header(&mut out, Rect { x: reason, y: status, w: s.width, h: s.height }, ENC_EXT_DESKTOP_SIZE);
                        out.extend_from_slice(&[1, 0, 0, 0]); // un écran
                        out.extend_from_slice(&0u32.to_be_bytes()); // identifiant
                        out.extend_from_slice(&[0, 0, 0, 0]); // position
                        out.extend_from_slice(&s.width.to_be_bytes());
                        out.extend_from_slice(&s.height.to_be_bytes());
                        out.extend_from_slice(&0u32.to_be_bytes()); // drapeaux
                    }
                    if plain_size {
                        header(&mut out, full, ENC_DESKTOP_SIZE);
                    }
                    for r in rects {
                        pixels.clear();
                        s.convert(r, &mut pixels);
                        if s.zlib {
                            header(&mut out, r, ENC_ZLIB);
                            let mut comp = Vec::with_capacity(pixels.len() / 4 + 64);
                            deflate(&mut z, &pixels, &mut comp)?;
                            out.extend_from_slice(&(comp.len() as u32).to_be_bytes());
                            out.extend_from_slice(&comp);
                        } else {
                            header(&mut out, r, ENC_RAW);
                            out.extend_from_slice(&pixels);
                        }
                    }
                }
            }
            out
        };
        if !msg.is_empty() {
            w.write_all(&msg).await?;
            w.flush().await?;
        }
    }
}

fn header(out: &mut Vec<u8>, r: Rect, enc: i32) {
    out.extend_from_slice(&r.x.to_be_bytes());
    out.extend_from_slice(&r.y.to_be_bytes());
    out.extend_from_slice(&r.w.to_be_bytes());
    out.extend_from_slice(&r.h.to_be_bytes());
    out.extend_from_slice(&enc.to_be_bytes());
}

/// Compression Zlib en flux continu (le client garde le même décompresseur d'un
/// rectangle à l'autre, comme l'exige l'encodage Zlib de RFB).
fn deflate(z: &mut Compress, input: &[u8], out: &mut Vec<u8>) -> io::Result<()> {
    let mut pos = 0;
    loop {
        if out.capacity() - out.len() < 4096 {
            out.reserve(64 * 1024);
        }
        let before_in = z.total_in();
        z.compress_vec(&input[pos..], out, FlushCompress::Sync)
            .map_err(|e| io::Error::other(e.to_string()))?;
        pos += (z.total_in() - before_in) as usize;
        if pos >= input.len() && out.len() < out.capacity() {
            return Ok(());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn texte_presse_papiers_flux_non_termine() {
        // Comme noVNC : zlib avec vidage « sync », sans fin de flux.
        let text = "été € ✓\r\nligne 2\0";
        let mut raw = (text.len() as u32).to_be_bytes().to_vec();
        raw.extend_from_slice(text.as_bytes());
        let mut z = Compress::new(Compression::default(), true);
        let mut out = Vec::with_capacity(256);
        z.compress_vec(&raw, &mut out, FlushCompress::Sync).unwrap();
        assert_eq!(extended_text(&out).as_deref(), Some("été € ✓\nligne 2"));
    }
    use flate2::{Decompress, FlushDecompress};

    #[test]
    fn zlib_stream_roundtrip() {
        let mut z = Compress::new(Compression::fast(), true);
        let mut d = Decompress::new(true);
        for i in 0..3u8 {
            let data: Vec<u8> = (0..200_000u32).map(|x| (x as u8).wrapping_mul(i + 1)).collect();
            let mut comp = Vec::new();
            deflate(&mut z, &data, &mut comp).unwrap();
            let mut back = Vec::with_capacity(data.len());
            d.decompress_vec(&comp, &mut back, FlushDecompress::Sync).unwrap();
            assert_eq!(back, data, "rectangle {i}");
        }
    }

    #[test]
    fn update_copies_and_marks() {
        let st = new_state(4, 2);
        let mut s = st.0.lock().unwrap();
        let src: Vec<u8> = (0..32).collect(); // 4×2 pixels RGBA
        s.update(&src, 16, Rect { x: 1, y: 1, w: 2, h: 1 });
        assert_eq!(&s.rgba[20..28], &src[20..28]);
        assert_eq!(s.rgba[0], 0);
        assert_eq!(s.dirty, vec![Rect { x: 1, y: 1, w: 2, h: 1 }]);
    }
}
