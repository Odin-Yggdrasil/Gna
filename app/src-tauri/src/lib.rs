//! Application Gná.
//!
//! Au lancement, la fenêtre affiche l'écran de réglages local (dossier `ui/`).
//! Une fois le serveur choisi et son certificat validé, l'application démarre le
//! relais local (crate `gna-relay`) et la fenêtre charge l'interface de Gná à travers
//! lui. Les pages de Gná n'ont aucun accès aux commandes de l'application.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use gna_relay::{ConnectError, Handle, Upstream};
use serde::{Deserialize, Serialize};
use tauri::webview::DownloadEvent;
use tauri::{AppHandle, Manager, State, Url, WebviewUrl, WebviewWindowBuilder};

/// Serveur mémorisé (fichier `server.json` dans le dossier de configuration de l'application).
#[derive(Clone, Serialize, Deserialize)]
struct Saved {
    address: String,
    fingerprint: String,
    /// Port local du relais lors du dernier lancement (réutilisé si possible).
    #[serde(default)]
    port: u16,
}

#[derive(Default)]
struct AppState {
    relay: Mutex<Option<Handle>>,
    home: Mutex<Option<Url>>, // adresse de l'écran de réglages local
}

#[derive(Serialize)]
struct Probe {
    address: String, // adresse normalisée (https://hôte:port)
    fingerprint: String,
}

fn config_path(app: &AppHandle) -> Result<PathBuf, String> {
    let dir = app.path().app_config_dir().map_err(|e| e.to_string())?;
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    Ok(dir.join("server.json"))
}

#[tauri::command]
fn saved_server(app: AppHandle) -> Option<Saved> {
    let p = config_path(&app).ok()?;
    serde_json::from_slice(&std::fs::read(p).ok()?).ok()
}

#[tauri::command]
fn forget_server(app: AppHandle, state: State<'_, AppState>) -> Result<(), String> {
    if let Some(h) = state.relay.lock().unwrap().take() {
        h.stop();
    }
    match std::fs::remove_file(config_path(&app)?) {
        Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e.to_string()),
        _ => Ok(()),
    }
}

/// Relève l'empreinte du certificat présenté par le serveur (premier lancement).
#[tauri::command]
async fn probe_server(address: String) -> Result<Probe, String> {
    let up = Upstream::parse(&address)?;
    let fp = gna_relay::probe(&up).await.map_err(|e| e.to_string())?;
    Ok(Probe { address: up.display(), fingerprint: fp })
}

#[derive(Serialize)]
#[serde(tag = "status", rename_all = "snake_case")]
enum Connect {
    /// Relais prêt : la fenêtre peut charger cette adresse.
    Ready { url: String },
    /// Le certificat présenté n'est pas celui validé.
    CertChanged { fingerprint: String },
    Error { message: String },
}

/// Vérifie le serveur avec l'empreinte validée, mémorise le choix et démarre le relais.
#[tauri::command]
async fn connect_server(
    app: AppHandle,
    state: State<'_, AppState>,
    address: String,
    fingerprint: String,
) -> Result<Connect, String> {
    let up = Upstream::parse(&address)?;

    // Vérification préalable : on signale tout de suite un certificat qui a changé
    // plutôt que d'ouvrir une page d'erreur.
    match gna_relay::probe(&up).await {
        Ok(fp) if same_fp(&fp, &fingerprint) => {}
        Ok(fp) => return Ok(Connect::CertChanged { fingerprint: fp }),
        Err(ConnectError::CertMismatch(fp)) => return Ok(Connect::CertChanged { fingerprint: fp }),
        Err(e) => return Ok(Connect::Error { message: e.to_string() }),
    }

    // Même serveur qu'au lancement précédent : on reprend le même port local, pour que
    // les réglages gardés par la page (liés à l'adresse http://127.0.0.1:port) persistent.
    let previous = saved_server(app.clone()).filter(|s| s.address == up.display()).map(|s| s.port).unwrap_or(0);
    let preferred = state.relay.lock().unwrap().as_ref().map(|h| h.addr.port()).unwrap_or(previous);
    if let Some(old) = state.relay.lock().unwrap().take() {
        old.stop(); // libère le port avant de relancer le relais
    }

    let address = up.display();
    let back = app.clone();
    let on_settings: Arc<dyn Fn() + Send + Sync> = Arc::new(move || show_settings(&back));
    let handle = gna_relay::start(up, &fingerprint, preferred, Some(on_settings)).await.map_err(|e| e.to_string())?;
    let url = handle.url();

    let saved = Saved { address, fingerprint: fingerprint.clone(), port: handle.addr.port() };
    std::fs::write(config_path(&app)?, serde_json::to_vec_pretty(&saved).unwrap()).map_err(|e| e.to_string())?;
    if let Some(old) = state.relay.lock().unwrap().replace(handle) {
        old.stop();
    }
    Ok(Connect::Ready { url })
}

fn same_fp(a: &str, b: &str) -> bool {
    let n = |s: &str| s.chars().filter(|c| c.is_ascii_hexdigit()).collect::<String>().to_ascii_uppercase();
    n(a) == n(b)
}

/// Ramène la fenêtre à l'écran de réglages local (appelé depuis la page d'erreur du relais).
fn show_settings(app: &AppHandle) {
    let state = app.state::<AppState>();
    let home = state.home.lock().unwrap().clone();
    if let (Some(mut home), Some(win)) = (home, app.get_webview_window("main")) {
        home.set_query(Some("settings=1"));
        let _ = win.navigate(home);
    }
}

/// Téléchargements (fichiers SFTP, transferts RDP) : enregistrés dans le dossier
/// Téléchargements, sans écraser un fichier existant (« nom (1).ext »…), puis un
/// message s'affiche dans la page.
fn on_download<R: tauri::Runtime>(webview: tauri::Webview<R>, event: DownloadEvent<'_>) -> bool {
    if let DownloadEvent::Finished { path, success, .. } = event {
        let msg = match (success, path) {
            (true, Some(p)) => format!("Fichier enregistré : {}", p.display()),
            (true, None) => "Fichier enregistré dans Téléchargements".to_string(),
            (false, _) => "Échec du téléchargement".to_string(),
        };
        let js = format!(
            r#"(()=>{{const d=document.createElement("div");d.textContent={};d.style.cssText="position:fixed;left:50%;bottom:24px;transform:translateX(-50%);z-index:99999;padding:10px 16px;border-radius:8px;background:#1f2630;color:#e8edf2;font:14px system-ui,sans-serif;box-shadow:0 4px 16px #0006;max-width:90vw;overflow-wrap:anywhere";document.body.appendChild(d);setTimeout(()=>d.remove(),5000)}})()"#,
            serde_json::to_string(&msg).unwrap()
        );
        let _ = webview.eval(js);
    }
    true
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .manage(AppState::default())
        .setup(|app| {
            let builder = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("Gná")
                .on_download(on_download);
            #[cfg(desktop)]
            let builder = builder.inner_size(1200.0, 800.0).min_inner_size(380.0, 500.0).center();
            let win = builder.build()?;
            if let Ok(url) = win.url() {
                *app.state::<AppState>().home.lock().unwrap() = Some(url);
            }
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![saved_server, forget_server, probe_server, connect_server])
        .run(tauri::generate_context!())
        .expect("erreur au démarrage de l'application Gná");
}
