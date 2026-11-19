#!/usr/bin/env bash
# Compile l'installeur Windows (.exe NSIS) depuis Linux, sans machine Windows.
#
# Prérequis (Debian / Ubuntu) :
#   sudo apt install mingw-w64 nsis
#   rustup target add x86_64-pc-windows-gnu
#
# Si « rustup target add » est impossible (pas d'accès à static.rust-lang.org),
# lancer avec BUILD_STD=1 : la bibliothèque standard Windows est alors compilée
# à partir des sources de Rust (dossier lib/rustlib/src/rust du sysroot).
#
# Résultat : src-tauri/target/x86_64-pc-windows-gnu/release/bundle/nsis/Gná_<version>_x64-setup.exe
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET=x86_64-pc-windows-gnu
export CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc

# Sous Windows/MinGW, la bibliothèque partagée (cdylib, utile à Android seulement)
# dépasse la limite de 65 535 symboles exportés : on la retire le temps de la compilation.
CARGO_TOML=src-tauri/Cargo.toml
cp "$CARGO_TOML" "$CARGO_TOML.orig"
trap 'mv "$CARGO_TOML.orig" "$CARGO_TOML"' EXIT
sed -i 's/^crate-type = .*/crate-type = ["rlib"]/' "$CARGO_TOML"

EXTRA=()
if [[ "${BUILD_STD:-0}" == 1 ]]; then
  export RUSTC_BOOTSTRAP=1
  EXTRA=(-- -Zbuild-std=std,panic_abort)
  # Le CLI de Tauri vérifie la cible auprès de rustup : on la signale comme installée.
  FAKE="$(mktemp -d)"
  cat > "$FAKE/rustup" <<EOF
#!/bin/sh
if [ "\$1 \$2" = "target list" ]; then
  $(command -v rustup) "\$@" | sed 's/^$TARGET\$/$TARGET (installed)/'
  [ "\$3" = "--installed" ] && echo $TARGET
  exit 0
fi
exec $(command -v rustup) "\$@"
EOF
  chmod +x "$FAKE/rustup"
  export PATH="$FAKE:$PATH"
fi

npx tauri build --target "$TARGET" --bundles nsis "${EXTRA[@]}"
