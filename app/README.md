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

### Windows, Linux et Android (GitHub Actions)

Le fichier `.github/workflows/app.yml` compile les trois versions sur GitHub :

1. Pousser le projet sur un dépôt GitHub.
2. Onglet **Actions** → **Application Gná** → **Run workflow** (ou pousser une étiquette `app-v0.1.0`).
3. Récupérer les fichiers dans les **Artifacts** de l'exécution : `.deb`/`.AppImage`, `.exe`/`.msi`, `.apk`.

Pour un APK Android signé avec ta propre clé (sinon : APK de débogage, installable mais signé avec une clé de test) :

```bash
keytool -genkey -v -keystore gna.jks -alias gna -keyalg RSA -keysize 4096 -validity 10000
base64 -w0 gna.jks   # à coller dans le secret ANDROID_KEYSTORE_B64
```

Secrets à créer (Settings → Secrets and variables → Actions) : `ANDROID_KEYSTORE_B64`, `ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS`.

Sur Android, le script `scripts/android-patch.sh` (lancé par le workflow) autorise le HTTP en clair **uniquement** vers `127.0.0.1`.

## Structure

```
relay/        relais local (Rust) : HTTP + WebSocket, épinglage du certificat
src-tauri/    application Tauri 2 (commandes, fenêtre, configuration)
ui/           écran local : choix du serveur et validation de l'empreinte
scripts/      configuration du projet Android
```
