#!/usr/bin/env bash
# Compile et signe l'APK Android de Gná sur ta machine (Linux).
#
# Usage :
#   GNA_KEYSTORE=~/chemin/gna-android.jks ./scripts/build-android.sh
#   (le mot de passe du keystore est demandé, ou lu dans GNA_KEYSTORE_PASSWORD)
#
# Résultat : app/Gna-<version>-android.apk (téléphones ARM 64 et 32 bits + émulateurs x86_64)
#
# Prérequis (une seule fois) : voir « Android » dans app/README.md
#   - JDK 17, Android SDK (ANDROID_HOME) avec platform-tools, build-tools et NDK (NDK_HOME)
#   - rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android i686-linux-android
#   - Node.js 20+ et « npm ci » dans app/
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "Erreur : $*" >&2; exit 1; }

VERSION=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' src-tauri/tauri.conf.json | head -1)
KEYSTORE="${GNA_KEYSTORE:-}"
ALIAS="${GNA_KEY_ALIAS:-gna}"

# ---------- vérifications ----------
[[ -n "$KEYSTORE" ]] || die "indiquer le keystore : GNA_KEYSTORE=/chemin/gna-android.jks $0"
[[ -f "$KEYSTORE" ]] || die "keystore introuvable : $KEYSTORE"
[[ -n "${ANDROID_HOME:-}" && -d "$ANDROID_HOME" ]] || die "ANDROID_HOME n'est pas défini (dossier du SDK Android)"
if [[ -z "${NDK_HOME:-}" ]]; then
  NDK_HOME=$(ls -d "$ANDROID_HOME"/ndk/* 2>/dev/null | sort -V | tail -1 || true)
  export NDK_HOME
fi
[[ -n "$NDK_HOME" && -d "$NDK_HOME" ]] || die "NDK introuvable : sdkmanager \"ndk;27.0.12077973\" puis relancer"
BT=$(ls -d "$ANDROID_HOME"/build-tools/* 2>/dev/null | sort -V | tail -1 || true)
[[ -x "$BT/apksigner" && -x "$BT/zipalign" ]] || die "build-tools absents : sdkmanager \"build-tools;35.0.0\""
command -v java >/dev/null || die "Java (JDK 17) absent : sudo apt install openjdk-17-jdk"
for t in aarch64-linux-android armv7-linux-androideabi x86_64-linux-android; do
  rustup target list --installed | grep -qx "$t" || die "cible Rust manquante : rustup target add $t"
done
[[ -d node_modules ]] || npm ci

if [[ -z "${GNA_KEYSTORE_PASSWORD:-}" ]]; then
  read -rsp "Mot de passe du keystore : " GNA_KEYSTORE_PASSWORD; echo
fi
export GNA_KEYSTORE_PASSWORD   # transmis à apksigner par variable, jamais en argument

# ---------- projet Android (généré une fois, puis adapté) ----------
if [[ ! -d src-tauri/gen/android ]]; then
  npx tauri android init --ci
fi
bash scripts/android-patch.sh

# ---------- compilation ----------
npx tauri android build --apk --target aarch64 --target armv7 --target x86_64

APK=$(find src-tauri/gen/android/app/build/outputs/apk -name '*-unsigned.apk' -newer src-tauri/tauri.conf.json | head -1)
[[ -n "$APK" ]] || APK=$(find src-tauri/gen/android/app/build/outputs/apk -name '*-unsigned.apk' | head -1)
[[ -n "$APK" ]] || die "APK non trouvé après la compilation"

# ---------- signature ----------
OUT="Gna-${VERSION}-android.apk"
TMP=$(mktemp --suffix=.apk)
trap 'rm -f "$TMP"' EXIT
"$BT/zipalign" -f 4 "$APK" "$TMP"
"$BT/apksigner" sign --ks "$KEYSTORE" --ks-key-alias "$ALIAS" \
  --ks-pass env:GNA_KEYSTORE_PASSWORD --key-pass env:GNA_KEYSTORE_PASSWORD --out "$OUT" "$TMP"
"$BT/apksigner" verify "$OUT"

echo
echo "APK signé : app/$OUT"
echo "Ajouter à la Release GitHub :  gh release upload v$VERSION app/$OUT"
echo "             (ou sur github.com : Releases → v$VERSION → Edit → déposer le fichier)"
