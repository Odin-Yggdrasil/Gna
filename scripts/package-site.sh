#!/usr/bin/env bash
# Construit le site puis l'archive à copier sur le serveur web :
#   clients/installateurs/gna-site_<version>.zip
# Sur le serveur : unzip gna-site_<version>.zip && sudo ./gna-site/installer.sh
# Les fichiers d’installation sont proposés au téléchargement ; GNA_SITE_TELECHARGEMENT=0 les retire.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

python3 -I scripts/build-site.py
version=$(ls clients/installateurs/gna-server_*_amd64.deb | sort -V | tail -1 | sed -E 's/.*_([0-9.]+)_amd64\.deb/\1/')

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
d="$work/gna-site"
mkdir -p "$d"
rsync -a --exclude deploy/ site/ "$d/site/"
install -m 755 site/deploy/installer.sh "$d/installer.sh"
install -m 644 site/deploy/nginx.conf "$d/nginx.conf"
install -m 644 site/deploy/README.md "$d/README-publication.md"
cat > "$d/LISEZMOI.txt" <<EOF
Site de présentation de Gná $version
=====================================

Sur le serveur web (Debian ou Ubuntu) :

    unzip gna-site_$version.zip
    cd gna-site
    sudo ./installer.sh

Réglages possibles (variables avant la commande) :

    DOMAINE   nom du site                     (défaut : gna.gjallarbru.ovh)
    NPM_IP    adresse de Nginx Proxy Manager  (défaut : 10.0.66.1 ; « aucun » = accès direct)
    RACINE    dossier des fichiers            (défaut : /var/www/gna-web)

Exemple : sudo DOMAINE=gna.exemple.fr NPM_IP=10.0.66.1 ./installer.sh

Le script installe nginx si besoin, copie le site, installe la configuration
(/etc/nginx/sites-available/gna-web), la teste avant de recharger nginx, puis
vérifie que le site répond. Pour mettre le site à jour : même commande avec la
nouvelle archive. Détails : README-publication.md.
EOF

mkdir -p clients/installateurs
out="clients/installateurs/gna-site_${version}.zip"
rm -f "$out"
(cd "$work" && zip -qr -X "$root/$out" gna-site)
ls -lh "$out"
