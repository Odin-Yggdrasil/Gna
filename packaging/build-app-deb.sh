#!/usr/bin/env bash
# Compile l'application Gná (Tauri) pour Linux dans un conteneur Debian Trixie, sans
# installer les paquets de développement WebKit sur la machine (même image qu'Andvari).
# Résultat : clients/installateurs/gna-app_<version>_amd64.deb (et l'AppImage si possible)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
image=andvari-tauri-build
# Compilation Rust hors du dossier synchronisé (plusieurs Go).
target="${CARGO_TARGET_DIR:-$HOME/.cache/gna/target}"
bundles="${BUNDLES:-deb}"
mkdir -p "$target" "$root/clients/installateurs"

docker image inspect "$image" >/dev/null 2>&1 || docker build -t "$image" "$root/../andvari/packaging/docker"

# Repartir de dossiers de paquets vides pour ne copier que la version produite.
rm -rf "$target/release/bundle/deb" "$target/release/bundle/appimage"

docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home \
  -e CARGO_HOME=/cargo -e RUSTUP_HOME=/rustup -e PATH=/cargo/bin:/usr/local/bin:/usr/bin:/bin \
  -e CARGO_TARGET_DIR=/target \
  -v "${CARGO_HOME:-$HOME/.cargo}:/cargo" \
  -v "${RUSTUP_HOME:-$HOME/.rustup}:/rustup:ro" \
  -v "$target:/target" \
  -v "$root:/src" \
  -w /src/clients/app \
  "$image" \
  bash -c "[ -d node_modules ] || npm ci; npx tauri build --bundles $bundles"

find "$target/release/bundle" -maxdepth 2 -type f \( -name '*.deb' -o -name '*.AppImage' \) \
  -exec cp {} "$root/clients/installateurs/" \; -print
