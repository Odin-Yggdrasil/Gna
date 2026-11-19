#!/usr/bin/env bash
# Compile l'installeur Windows de l'application Gná (NSIS) depuis Linux, dans un conteneur
# (voir docker/windows.Dockerfile) : ni MinGW, ni NSIS, ni cible Rust à installer sur la machine.
# Résultat : clients/installateurs/Gna-<version>-windows-setup.exe
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
image=gna-windows-build
target="${CARGO_TARGET_DIR:-$HOME/.cache/gna/target-windows}"
mkdir -p "$target" "$HOME/.cache/gna/cargo-registry" "$root/clients/installateurs"

docker image inspect "$image" >/dev/null 2>&1 || docker build -t "$image" -f "$root/packaging/docker/windows.Dockerfile" "$root/packaging/docker"

version="$(grep -m1 '"version"' "$root/clients/app/src-tauri/tauri.conf.json" | sed 's/.*: "\(.*\)".*/\1/')"
rm -rf "$target/x86_64-pc-windows-gnu/release/bundle/nsis"

docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/home -e CARGO_TARGET_DIR=/target \
  -v "$HOME/.cache/gna/cargo-registry:/opt/cargo/registry" \
  -v "$target:/target" \
  -v "$root:/src" \
  -w /src/clients/app \
  "$image" \
  bash -c "[ -d node_modules ] || npm ci; ./scripts/build-windows.sh"

exe="$(find "$target/x86_64-pc-windows-gnu/release/bundle/nsis" -name '*-setup.exe' | head -1)"
cp "$exe" "$root/clients/installateurs/Gna-$version-windows-setup.exe"
ls -l "$root/clients/installateurs/Gna-$version-windows-setup.exe"
