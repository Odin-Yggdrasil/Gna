#!/usr/bin/env bash
# À lancer après « npx tauri android init ».
# Autorise le HTTP en clair uniquement vers 127.0.0.1 (le relais local de l'application) ;
# tout le reste du trafic reste en HTTPS obligatoire.
set -euo pipefail
cd "$(dirname "$0")/.."
MAIN=src-tauri/gen/android/app/src/main
[[ -f $MAIN/AndroidManifest.xml ]] || { echo "Projet Android absent : lancer d'abord « npx tauri android init »" >&2; exit 1; }

mkdir -p "$MAIN/res/xml"
cat > "$MAIN/res/xml/network_security_config.xml" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<network-security-config>
    <base-config cleartextTrafficPermitted="false" />
    <domain-config cleartextTrafficPermitted="true">
        <domain includeSubdomains="false">127.0.0.1</domain>
    </domain-config>
</network-security-config>
XML

# Remplace l'attribut usesCleartextTraffic du modèle Tauri par la configuration ci-dessus.
sed -i -E 's#android:usesCleartextTraffic="[^"]*"#android:networkSecurityConfig="@xml/network_security_config"#' "$MAIN/AndroidManifest.xml"
grep -q 'networkSecurityConfig' "$MAIN/AndroidManifest.xml" || \
  sed -i 's#<application#<application android:networkSecurityConfig="@xml/network_security_config"#' "$MAIN/AndroidManifest.xml"
echo "Projet Android configuré (HTTP en clair limité à 127.0.0.1)."
