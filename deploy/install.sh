#!/usr/bin/env bash
# Installation native de Gná (Debian / Ubuntu), sans conteneur.
# Usage : make build && sudo ./deploy/install.sh
# Relançable sans risque : il ne remplace ni la configuration ni la base existantes.
set -euo pipefail

SRC="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$SRC/bin/gna"

info() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mErreur :\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "à lancer avec sudo"
[[ -x "$BIN" ]] || die "binaire introuvable ($BIN) : lancer d'abord « make build »"

# 1. PostgreSQL
if ! dpkg -s postgresql >/dev/null 2>&1; then
  info "Installation de PostgreSQL"
  apt-get update -qq
  apt-get install -y -qq postgresql
fi
systemctl enable --now postgresql >/dev/null

# 2. Utilisateur système dédié, sans shell ni mot de passe
if ! id gna >/dev/null 2>&1; then
  info "Création de l'utilisateur système gna"
  useradd --system --user-group --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin gna
fi

# 3. Rôle et base PostgreSQL (authentification « peer » via socket Unix : pas de mot de passe)
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='gna'" | grep -q 1; then
  info "Création du rôle PostgreSQL gna"
  runuser -u postgres -- createuser gna
fi
if ! runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname='gna'" | grep -q 1; then
  info "Création de la base gna"
  runuser -u postgres -- createdb -O gna gna
fi

# 4. Binaire et documentation
info "Installation du binaire dans /usr/local/bin"
install -m 0755 "$BIN" /usr/local/bin/gna
if [[ -x "$SRC/bin/gna-rdp" ]]; then
  info "Installation de la passerelle RDP dans /usr/local/bin"
  install -m 0755 "$SRC/bin/gna-rdp" /usr/local/bin/gna-rdp
else
  info "Passerelle RDP absente : les cibles RDP ne seront pas disponibles"
fi
install -D -m 0644 "$SRC/README.md" /usr/local/share/doc/gna/README.md

# 5. Configuration : créée une seule fois, avec une clé maître neuve
install -d -m 0750 -o root -g gna /etc/gna
if [[ ! -f /etc/gna/gna.env ]]; then
  info "Création de /etc/gna/gna.env avec une nouvelle clé maître"
  key="$(/usr/local/bin/gna keygen)"
  sed "s|^GNA_MASTER_KEY=.*|GNA_MASTER_KEY=$key|" "$SRC/deploy/gna.env.example" > /etc/gna/gna.env
  chown root:gna /etc/gna/gna.env
  chmod 0640 /etc/gna/gna.env
  NEW_KEY=1
else
  info "/etc/gna/gna.env existe déjà : conservé"
fi

# 6. Service systemd
info "Installation du service systemd"
install -m 0644 "$SRC/deploy/gna.service" /etc/systemd/system/gna.service
systemctl daemon-reload
systemctl enable gna >/dev/null
systemctl restart gna
sleep 1
systemctl is-active --quiet gna || { journalctl -u gna -n 20 --no-pager; die "le service n'a pas démarré"; }

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
PORT="$(sed -n 's/^GNA_LISTEN=.*:\([0-9]*\)$/\1/p' /etc/gna/gna.env | tail -1)"
SCHEME=http; grep -qE '^GNA_TLS_(SELF_SIGNED=true|CERT=.+)' /etc/gna/gna.env && SCHEME=https

echo
info "Gná est démarré."
echo "  Créer le premier administrateur :  sudo -u gna gna user add admin --admin"
echo "  Ouvrir :                           $SCHEME://${IP:-<ip-du-serveur>}:${PORT:-8443}"
echo "  Empreinte du certificat :          sudo -u gna gna tls info"
echo "  Journaux :                         journalctl -u gna -f"
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  echo
  echo "  Pare-feu ufw actif : sudo ufw allow ${PORT:-8443}/tcp"
fi
if [[ "${NEW_KEY:-0}" == 1 ]]; then
  echo
  printf '\033[1;33m  Sauvegardez la clé maître (GNA_MASTER_KEY dans /etc/gna/gna.env) hors de ce serveur.\033[0m\n'
fi
