<p align="center"><img src="../deploy/logo/gna-180.png" alt="Gná" width="96"></p>

# Application Gná (Windows, Linux, Android)

L'application affiche l'interface de ton serveur Gná dans sa propre fenêtre, avec le **certificat du serveur épinglé**.

## Fonctionnement

Les moteurs web intégrés aux applications refusent les certificats auto-signés. L'application démarre donc un **relais local** (sur `127.0.0.1`, inaccessible depuis le réseau) : la fenêtre charge l'interface de Gná à travers lui, et le relais transmet tout au serveur en HTTPS.

```
Fenêtre de l'app ──HTTP local──► relais (127.0.0.1) ──HTTPS, certificat épinglé──► serveur Gná
```

- **Premier lancement** : saisir l'adresse du serveur (ex. `10.0.1.20`, port 8443 par défaut). L'application affiche l'empreinte du certificat : la comparer avec `sudo -u gna gna tls info` sur le serveur, puis valider.
- **Lancements suivants** : connexion directe au serveur mémorisé.
- **Certificat modifié** : connexion bloquée, les deux empreintes sont affichées ; la nouvelle n'est acceptée qu'après confirmation explicite.
- **Téléchargements** (fichiers SFTP, transferts RDP) : enregistrés dans le dossier Téléchargements, sans écraser un fichier existant.
- Le relais réutilise le même port local d'un lancement à l'autre (réglages de la page conservés).
- Le relais refuse toute requête qui ne vise pas exactement son adresse (protection contre le « DNS rebinding »), et les pages de Gná n'ont aucun accès aux fonctions de l'application.

## Compiler

### Linux (sur la machine)

```bash
sudo apt install libwebkit2gtk-4.1-dev libayatana-appindicator3-dev librsvg2-dev libxdo-dev libssl-dev
# Rust : https://rustup.rs   —   Node.js 20+
cd app
npm ci
npx tauri build --bundles deb,appimage
```

Résultat dans `src-tauri/target/release/bundle/`. Sous Linux, le paquet s'appelle `gna-app` (le paquet `gna` est le serveur).

### Windows, depuis Linux

```bash
sudo apt install mingw-w64 nsis
rustup target add x86_64-pc-windows-gnu
cd app && npm ci
./scripts/build-windows.sh            # BUILD_STD=1 si la cible ne peut pas être téléchargée
```

Résultat : `src-tauri/target/x86_64-pc-windows-gnu/release/bundle/nsis/Gná_<version>_x64-setup.exe` (installeur en français, installé pour l'utilisateur courant, sans droits administrateur ; WebView2 est installé automatiquement s'il manque, ce qui est rare sous Windows 10 et 11).

### Toutes les versions + publication (GitHub Actions)

Le fichier `.github/workflows/release.yml` compile **le serveur et les applications Linux, Windows et Android** sur GitHub, puis publie une **Release** avec tous les fichiers d'installation :

```bash
# mettre à jour VERSION, app/package.json et app/src-tauri/tauri.conf.json, puis :
git tag v0.13.0 && git push origin v0.13.0
```

Sans étiquette (Actions → **Publication** → **Run workflow**), les fichiers sont seulement disponibles dans les **Artifacts** de l'exécution.

**Android** : l'APK est signé avec ta clé (`gna-android.jks`, à conserver précieusement : sans elle, impossible de publier une mise à jour installable par-dessus). Secrets à créer (Settings → Secrets and variables → Actions) :

| Secret | Valeur |
|---|---|
| `ANDROID_KEYSTORE_B64` | résultat de `base64 -w0 gna-android.jks` |
| `ANDROID_KEYSTORE_PASSWORD` | mot de passe du keystore |
| `ANDROID_KEY_ALIAS` | `gna` |

Sans ces secrets, la publication se fait sans APK. Le script `scripts/android-patch.sh` (lancé par le workflow) autorise le HTTP en clair **uniquement** vers `127.0.0.1`, installe les icônes et confie les téléchargements au gestionnaire de téléchargements d'Android (dossier « Download »).

## Structure

```
relay/        relais local (Rust) : HTTP + WebSocket, épinglage du certificat
src-tauri/    application Tauri 2 (commandes, fenêtre, configuration)
ui/           écran local : choix du serveur et validation de l'empreinte
scripts/      configuration du projet Android
```
