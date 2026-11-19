#!/usr/bin/env bash
# Compile le serveur gna (Go, binaire statique) et la passerelle RDP gna-rdp (Rust)
# dans dist/linux, après les tests. Aucune étape front-end : l'interface est servie telle quelle.
# Avec --app, compile aussi l'application Tauri (clients/app).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
version="${VERSION:-$(cat "$root/VERSION")}"
out="$root/dist/linux"
mkdir -p "$out"

echo "== Serveur"
cd "$root/server"
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out/gna" ./cmd/gna

# Passerelle RDP : compilée si Rust ≥ 1.89 est présent (rustup), sinon le binaire
# précompilé fourni (Linux x86-64, glibc ≥ 2.34 : Debian 12 et 13, Ubuntu 22.04 et plus) est utilisé.
echo "== Passerelle RDP"
if command -v cargo >/dev/null 2>&1 && cargo --version | awk '{split($2,v,"."); exit !(v[1]>1 || (v[1]==1 && v[2]>=89))}'; then
  (cd "$root/server/rdpbridge" && cargo build --release --locked && cp target/release/gna-rdp "$out/gna-rdp")
elif [ "$(uname -m)" = "x86_64" ] && [ -f "$root/packaging/prebuilt/gna-rdp-linux-amd64" ]; then
  echo "Rust ≥ 1.89 absent : utilisation de la passerelle RDP précompilée."
  install -m 0755 "$root/packaging/prebuilt/gna-rdp-linux-amd64" "$out/gna-rdp"
else
  echo "Attention : passerelle RDP non disponible (installer Rust avec https://rustup.rs). Le RDP sera désactivé."
fi

if [ "${1:-}" = "--app" ]; then
  echo "== Application (Tauri, $(uname -s))"
  (cd "$root/clients/app" && npx tauri build)
  find "$root/clients/app/src-tauri/target/release/bundle" -maxdepth 2 -type f \
    \( -name '*.deb' -o -name '*.AppImage' -o -name '*.exe' \) -exec cp {} "$root/clients/installateurs/" \; -print
fi
ls -lh "$out"
