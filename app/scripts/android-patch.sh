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
# Icônes de l'application (logo Gná) dans le projet Android.
npx tauri icon app-icon.png >/dev/null

# Téléchargements (fichiers SFTP, transferts RDP) : le WebView Android les ignore par
# défaut. On les confie au gestionnaire de téléchargements d'Android, qui les enregistre
# dans le dossier « Download » du téléphone (avec le cookie de session de la page).
ACT=$(find "$MAIN/java" -name MainActivity.kt | head -1)
PKG=$(sed -n 's/^package //p' "$ACT")
cat > "$ACT" <<KT
package $PKG

import android.app.DownloadManager
import android.net.Uri
import android.os.Environment
import android.webkit.CookieManager
import android.webkit.URLUtil
import android.webkit.WebView
import android.widget.Toast

class MainActivity : TauriActivity() {
  override fun onWebViewCreate(webView: WebView) {
    webView.setDownloadListener { url, userAgent, contentDisposition, mimeType, _ ->
      try {
        val name = URLUtil.guessFileName(url, contentDisposition, mimeType)
        val req = DownloadManager.Request(Uri.parse(url))
          .setTitle(name)
          .setMimeType(mimeType)
          .addRequestHeader("User-Agent", userAgent)
          .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
          .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, name)
        CookieManager.getInstance().getCookie(url)?.let { req.addRequestHeader("Cookie", it) }
        (getSystemService(DOWNLOAD_SERVICE) as DownloadManager).enqueue(req)
        Toast.makeText(this, "Téléchargement de \"\$name\" dans Download", Toast.LENGTH_SHORT).show()
      } catch (e: Exception) {
        Toast.makeText(this, "Échec du téléchargement : \${e.message}", Toast.LENGTH_LONG).show()
      }
    }
  }
}
KT

echo "Projet Android configuré (HTTP en clair limité à 127.0.0.1, icônes, téléchargements)."
