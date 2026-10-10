#!/usr/bin/env bash
# Construit le paquet Debian du serveur Gná : dist/gna_<version>_amd64.deb
# Usage : make deb   (ou ./deploy/deb/build.sh <version> après « make build »)
set -euo pipefail

VERSION="${1:?version attendue}"
SRC="$(cd "$(dirname "$0")/../.." && pwd)"
ARCH=amd64
[[ -x "$SRC/bin/gna" ]] || { echo "bin/gna absent : lancer « make build »" >&2; exit 1; }
[[ -x "$SRC/bin/gna-rdp" ]] || { echo "bin/gna-rdp absent : lancer « make build »" >&2; exit 1; }

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

install -D -m 0755 "$SRC/bin/gna"     "$ROOT/usr/bin/gna"
install -D -m 0755 "$SRC/bin/gna-rdp" "$ROOT/usr/bin/gna-rdp"
# Service : mêmes protections que l'installation manuelle, binaire dans /usr/bin.
install -d "$ROOT/usr/lib/systemd/system"
sed -e 's|/usr/local/bin/gna|/usr/bin/gna|' -e 's|/usr/local/share/doc/gna|/usr/share/doc/gna|' \
  "$SRC/deploy/gna.service" > "$ROOT/usr/lib/systemd/system/gna.service"
chmod 0644 "$ROOT/usr/lib/systemd/system/gna.service"
install -D -m 0644 "$SRC/README.md"              "$ROOT/usr/share/doc/gna/README.md"
install -D -m 0644 "$SRC/deploy/gna.env.example" "$ROOT/usr/share/gna/gna.env.example"
install -D -m 0644 "$SRC/deploy/logo/gna-512.png" "$ROOT/usr/share/icons/hicolor/512x512/apps/gna.png"
install -D -m 0644 "$SRC/deploy/logo/favicon.svg" "$ROOT/usr/share/icons/hicolor/scalable/apps/gna.svg"

install -d "$ROOT/DEBIAN"
SIZE="$(du -sk --exclude=DEBIAN "$ROOT" | cut -f1)"
sed -e "s/@VERSION@/$VERSION/" -e "s/@ARCH@/$ARCH/" -e "s/@SIZE@/$SIZE/" "$SRC/deploy/deb/control" > "$ROOT/DEBIAN/control"
for f in postinst prerm postrm; do
  install -m 0755 "$SRC/deploy/deb/$f" "$ROOT/DEBIAN/$f"
done

mkdir -p "$SRC/dist"
OUT="$SRC/dist/gna_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group -Zxz --build "$ROOT" "$OUT" >/dev/null
echo "Paquet créé : $OUT"
