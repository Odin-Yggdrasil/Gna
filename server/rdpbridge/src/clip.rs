//! Presse-papiers RDP (canal CLIPRDR), texte uniquement.
//!
//! Le canal appelle ce module de façon synchrone ; les événements sont transmis à la
//! boucle de session, qui seule peut envoyer des messages sur la connexion RDP.

use tokio::sync::mpsc::UnboundedSender as Sender;

use ironrdp::cliprdr::backend::CliprdrBackend;
use ironrdp::cliprdr::pdu::{
    ClipboardFormat, ClipboardFormatId, ClipboardGeneralCapabilityFlags, FileContentsRequest, FileContentsResponse,
    FormatDataRequest, FormatDataResponse, LockDataId,
};
use ironrdp::core::impl_as_any;

pub const CF_TEXT: ClipboardFormatId = ClipboardFormatId::CF_TEXT;
pub const CF_UNICODETEXT: ClipboardFormatId = ClipboardFormatId::CF_UNICODETEXT;

/// Taille maximale d'un texte copié (dans un sens comme dans l'autre).
pub const MAX_TEXT: usize = 1024 * 1024;

#[derive(Debug)]
pub enum Event {
    /// Canal prêt : annoncer le contenu local (éventuellement vide).
    Ready,
    /// La machine distante redemande la liste des formats disponibles.
    FormatListRequested,
    /// Copie côté distant ; `Some(format)` si du texte est disponible.
    RemoteCopy(Option<ClipboardFormatId>),
    /// La machine distante colle : elle demande notre texte.
    DataRequested(ClipboardFormatId),
    /// Texte reçu de la machine distante.
    Data(Option<String>),
    /// La machine distante a pris en compte notre nouveau contenu.
    FormatListAck,
}

#[derive(Debug)]
pub struct Backend {
    tx: Sender<Event>,
    pending: Option<ClipboardFormatId>,
}

impl Backend {
    pub fn new(tx: Sender<Event>) -> Self {
        Backend { tx, pending: None }
    }
}

impl_as_any!(Backend);

impl CliprdrBackend for Backend {
    fn temporary_directory(&self) -> &str {
        ".cliprdr"
    }

    fn client_capabilities(&self) -> ClipboardGeneralCapabilityFlags {
        ClipboardGeneralCapabilityFlags::USE_LONG_FORMAT_NAMES
    }

    fn on_ready(&mut self) {
        let _ = self.tx.send(Event::Ready);
    }

    fn on_request_format_list(&mut self) {
        let _ = self.tx.send(Event::FormatListRequested);
    }

    fn on_format_list_response(&mut self, _ok: bool) {
        let _ = self.tx.send(Event::FormatListAck);
    }

    fn on_process_negotiated_capabilities(&mut self, _: ClipboardGeneralCapabilityFlags) {}

    fn on_remote_copy(&mut self, formats: &[ClipboardFormat]) {
        let pick = if formats.iter().any(|f| f.id == CF_UNICODETEXT) {
            Some(CF_UNICODETEXT)
        } else if formats.iter().any(|f| f.id == CF_TEXT) {
            Some(CF_TEXT)
        } else {
            None
        };
        self.pending = pick;
        let _ = self.tx.send(Event::RemoteCopy(pick));
    }

    fn on_format_data_request(&mut self, request: FormatDataRequest) {
        let _ = self.tx.send(Event::DataRequested(request.format));
    }

    fn on_format_data_response(&mut self, response: FormatDataResponse<'_>) {
        let text = if response.is_error() || response.data().len() > MAX_TEXT * 2 {
            None
        } else if self.pending == Some(CF_TEXT) {
            response.to_string().ok()
        } else {
            response.to_unicode_string().ok()
        };
        let text = text.map(|t| t.trim_end_matches('\0').to_string());
        let _ = self.tx.send(Event::Data(text));
    }

    fn on_file_contents_request(&mut self, _: FileContentsRequest) {}
    fn on_file_contents_response(&mut self, _: FileContentsResponse<'_>) {}
    fn on_lock(&mut self, _: LockDataId) {}
    fn on_unlock(&mut self, _: LockDataId) {}
}
