#!/usr/bin/env bash
# Construit le paquet Debian du serveur Gná : clients/installateurs/gna-server_<version>_amd64.deb
# Prérequis : ./scripts/build.sh (binaires Linux dans dist/linux). Usage : ./packaging/deb/build-deb.sh [version]
set -euo pipefail

SRC="$(cd "$(dirname "$0")/../.." && pwd)"
VERSION="${1:-${VERSION:-$(cat "$SRC/VERSION")}}"
HERE="$SRC/packaging/deb"
BIN="$SRC/dist/linux"
ARCH=amd64
[[ -x "$BIN/gna" ]] || { echo "dist/linux/gna absent : lancer ./scripts/build.sh" >&2; exit 1; }
[[ -x "$BIN/gna-rdp" ]] || { echo "dist/linux/gna-rdp absent : lancer ./scripts/build.sh" >&2; exit 1; }

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

install -D -m 0755 "$BIN/gna"     "$ROOT/usr/bin/gna"
install -D -m 0755 "$BIN/gna-rdp" "$ROOT/usr/bin/gna-rdp"
# Service : mêmes protections que l'installation manuelle, binaire dans /usr/bin.
install -d "$ROOT/usr/lib/systemd/system"
sed -e 's|/usr/local/bin/gna|/usr/bin/gna|' -e 's|/usr/local/share/doc/gna|/usr/share/doc/gna|' \
  "$HERE/gna.service" > "$ROOT/usr/lib/systemd/system/gna.service"
chmod 0644 "$ROOT/usr/lib/systemd/system/gna.service"
install -D -m 0644 "$SRC/README.md"              "$ROOT/usr/share/doc/gna/README.md"
install -D -m 0644 "$SRC/LICENSE"                "$ROOT/usr/share/doc/gna/copyright"   # AGPL-3.0
install -D -m 0644 "$SRC/THIRD-PARTY.md"         "$ROOT/usr/share/doc/gna/THIRD-PARTY.md"
install -D -m 0644 "$HERE/gna.env.example" "$ROOT/usr/share/gna/gna.env.example"
install -D -m 0644 "$SRC/assets/logo/bindrune/gna-512.png" "$ROOT/usr/share/icons/hicolor/512x512/apps/gna.png"
install -D -m 0644 "$SRC/assets/logo/bindrune/favicon.svg" "$ROOT/usr/share/icons/hicolor/scalable/apps/gna.svg"

install -d "$ROOT/DEBIAN"
SIZE="$(du -sk --exclude=DEBIAN "$ROOT" | cut -f1)"
sed -e "s/@VERSION@/$VERSION/" -e "s/@ARCH@/$ARCH/" -e "s/@SIZE@/$SIZE/" "$HERE/control" > "$ROOT/DEBIAN/control"
for f in postinst prerm postrm; do
  install -m 0755 "$HERE/$f" "$ROOT/DEBIAN/$f"
done

mkdir -p "$SRC/clients/installateurs"
OUT="$SRC/clients/installateurs/gna-server_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group -Zxz --build "$ROOT" "$OUT" >/dev/null
echo "Paquet créé : $OUT"
