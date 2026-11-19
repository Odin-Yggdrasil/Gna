#!/bin/sh
# Installe ou met à jour le site de présentation de Gná sur ce serveur (nginx).
#
#   sudo ./installer.sh                         valeurs par défaut ci-dessous
#   sudo DOMAINE=gna.exemple.fr NPM_IP=10.0.66.1 ./installer.sh
#   sudo NPM_IP=aucun ./installer.sh            site joignable directement (sans reverse proxy)
#
# Relancer le script avec une nouvelle archive met le site à jour.
set -eu

DOMAINE="${DOMAINE:-gna.gjallarbru.ovh}"   # nom du site (server_name)
NPM_IP="${NPM_IP:-10.0.66.1}"                 # seule adresse autorisée (Nginx Proxy Manager), ou « aucun »
RACINE="${RACINE:-/var/www/gna-web}"       # dossier des fichiers du site
NOM="gna-web"                              # nom de la configuration nginx

ici="$(cd "$(dirname "$0")" && pwd)"
site="$ici/site"

if [ "$(id -u)" -ne 0 ]; then
	echo "À lancer en root : sudo $0" >&2
	exit 1
fi
[ -f "$site/index.html" ] || { echo "Fichiers du site introuvables dans $site" >&2; exit 1; }

echo "== Gná : site de présentation"
echo "   domaine : $DOMAINE"
echo "   dossier : $RACINE"
echo "   accès   : $( [ "$NPM_IP" = aucun ] && echo "direct" || echo "réservé à $NPM_IP (Nginx Proxy Manager)")"

# 1. nginx
if ! command -v nginx >/dev/null 2>&1; then
	echo "== Installation de nginx"
	apt-get update -q
	DEBIAN_FRONTEND=noninteractive apt-get install -y -q nginx
fi

# 2. Fichiers du site (copie complète : les fichiers retirés de l'archive disparaissent aussi)
echo "== Copie des fichiers"
install -d -m 755 "$RACINE"
if command -v rsync >/dev/null 2>&1; then
	rsync -a --delete "$site/" "$RACINE/"
else
	find "$RACINE" -mindepth 1 -delete
	cp -a "$site/." "$RACINE/"
fi
chown -R root:root "$RACINE"
find "$RACINE" -type d -exec chmod 755 {} +
find "$RACINE" -type f -exec chmod 644 {} +

# 3. Vérification des fichiers proposés au téléchargement
if [ -f "$RACINE/telechargements/SHA256SUMS" ]; then
	(cd "$RACINE/telechargements" && sha256sum -c --quiet SHA256SUMS) && echo "   empreintes des fichiers vérifiées"
fi

# 4. Configuration nginx
echo "== Configuration nginx"
conf="/etc/nginx/sites-available/$NOM"
sed -e "s#server_name gna.gjallarbru.ovh;#server_name $DOMAINE;#" \
    -e "s#root /var/www/gna-web;#root $RACINE;#" \
    "$ici/nginx.conf" > "$conf.nouveau"
if [ "$NPM_IP" = aucun ]; then
	# Sans reverse proxy : pas de restriction d'adresse.
	sed -i -e '/^    allow /d' -e '/^    deny all;/d' "$conf.nouveau"
else
	sed -i -e "s#allow 10.0.66.1;#allow $NPM_IP;#" "$conf.nouveau"
fi
[ -f "$conf" ] && cp "$conf" "$conf.precedent"
mv "$conf.nouveau" "$conf"
ln -sf "$conf" "/etc/nginx/sites-enabled/$NOM"

if ! nginx -t 2>/tmp/gna-nginx-test.log; then
	cat /tmp/gna-nginx-test.log >&2
	echo "Configuration nginx refusée." >&2
	if [ -f "$conf.precedent" ]; then
		mv "$conf.precedent" "$conf"
		echo "Ancienne configuration remise." >&2
	else
		rm -f "/etc/nginx/sites-enabled/$NOM"
	fi
	exit 1
fi
rm -f /tmp/gna-nginx-test.log "$conf.precedent"
systemctl enable --now nginx >/dev/null 2>&1 || true
systemctl reload nginx

# 5. Contrôle
echo "== Contrôle"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $DOMAINE" http://127.0.0.1/ 2>/dev/null || echo "?")
echo "   http://127.0.0.1/ (Host: $DOMAINE) : $code"
if [ "$code" = 200 ]; then
	echo "Site installé."
else
	echo "Réponse inattendue : vérifiez « sudo nginx -T » et « sudo journalctl -u nginx »." >&2
fi
if [ "$NPM_IP" != aucun ]; then
	cat <<EOF

Dans Nginx Proxy Manager : Proxy Host « $DOMAINE », scheme http, adresse de ce serveur, port 80,
« Block Common Exploits », puis certificat Let's Encrypt avec Force SSL, HTTP/2 et HSTS.
Au pare-feu de ce serveur, n'ouvrez le port 80 qu'à $NPM_IP.
EOF
fi
