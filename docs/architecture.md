# Architecture de Gná

## Vue d'ensemble

```
 [navigateur]  ──HTTPS──┐
                        ├──→ [ gna (Go) ] ──SSH (clé d'hôte épinglée)──────────→ machines Linux
 [app Tauri] ─HTTP local─┘        │      ──VNC (RFB, mot de passe côté serveur)─→ bureaux VNC
   └ relais 127.0.0.1             │      ──tube──→ [ gna-rdp (Rust, IronRDP) ] ──RDP/NLA──→ Windows, xrdp
     (certificat épinglé)         │
                            [PostgreSQL]   enregistrements chiffrés : /var/lib/gna/recordings
```

Le navigateur ne parle jamais aux machines : Gná ouvre les connexions et relaie les flux par
WebSocket. Les secrets des cibles (mots de passe, clés privées) sont chiffrés en base et ne
reviennent jamais au navigateur.

Pas de conteneur : un binaire Go statique, la passerelle RDP, un service systemd durci
(utilisateur `gna` sans privilèges, système en lecture seule) et PostgreSQL local (socket Unix,
authentification *peer*).

## Serveur

| Élément        | Détail                                                                         |
|----------------|--------------------------------------------------------------------------------|
| HTTP           | `net/http` standard (routes Go 1.22), interface embarquée (`web/static`)       |
| Stockage       | PostgreSQL, migrations SQL appliquées au démarrage                             |
| Mots de passe  | Argon2id                                                                       |
| 2FA            | TOTP obligatoire (RFC 6238), enrôlement par QR code, codes non réutilisables  |
| Sessions       | cookie `HttpOnly`, `SameSite=Strict` (`__Host-` en HTTPS) ; seul le hash du jeton est stocké ; 12 h max, 30 min d'inactivité |
| Connexion      | verrouillage après 5 échecs (15 min), limitation de débit par IP               |
| Secrets        | AES-256-GCM avec la clé maître (`GNA_MASTER_KEY`)                              |
| Audit          | journal en ajout seul (un trigger PostgreSQL interdit modification et suppression) |
| HTTPS          | certificat auto-signé généré, certificat fourni, ou reverse proxy local         |
| Jetons d'accès | `Authorization: Bearer gna_…`, empreinte SHA-256 seule en base ; acceptés uniquement par `GET /api/me` et `GET /api/targets` (lecture seule), journalisés, limités en débit |
| Droits         | administrateurs ; accès aux cibles par utilisateur ou par groupe ; retrait appliqué aux sessions ouvertes en 30 s |

### Protocoles

| Protocole | Côté serveur                                         | Côté navigateur |
|-----------|------------------------------------------------------|-----------------|
| SSH       | `golang.org/x/crypto/ssh`, clé d'hôte épinglée ; SFTP sur la même connexion | xterm.js |
| VNC       | relais RFB, Gná s'authentifie lui-même auprès de la cible | noVNC |
| RDP       | `gna-rdp` (IronRDP) convertit l'écran RDP en flux VNC ; identifiants passés par un tube, certificat épinglé, lecteur partagé « Gna » | noVNC |

### Enregistrements

Sessions SSH (asciicast) et VNC/RDP (flux RFB) enregistrées et chiffrées par blocs
(AES-256-GCM) : un bloc modifié, déplacé ou supprimé est détecté. Si l'enregistrement est
impossible, la connexion est refusée. Relecture et recherche réservées aux administrateurs et
tracées dans le journal.

## Interface

HTML/CSS/JS sans étape de compilation, servis depuis le binaire, sans ressource externe, CSP
stricte. Bibliothèques embarquées : xterm.js, noVNC, asciinema-player (voir `THIRD-PARTY.md`).

**Charte commune avec Saga et Andvari** : fonds Nuit / Parchemin, Noto Sans, rayons 8-12 px,
barre du haut translucide, bindrune en tuile, anneau de runes sur l'écran de connexion. Accent
propre à Gná : bleu-vert `#5FB3A1`.

**Ergonomie commune avec Saga** : barre du haut à pages (`#/machines`, `#/sessions`,
`#/journal`, `#/parametres`), page Paramètres à menu latéral, dialogues `dlg-head` / `dlg-body` /
`dlg-foot`, notifications éphémères (`popover`, au-dessus des dialogues), bascule clair/sombre.

**Thèmes** (variables CSS sous `[data-theme]`, communs aux trois applications) : Gná, Écume,
Fjord, Saga, Vélin, Nuit, Parchemin, Braise, Givre, ou Automatique. Le choix est enregistré
dans le compte (`PUT /api/me/prefs`) et recopié dans le navigateur (`theme.js`) pour éviter un
flash au chargement. Terminaux et bureaux à distance suivent le thème (`--term-*`).

## Application (`clients/app`)

Tauri 2 pour Windows, Linux et Android. Les moteurs web intégrés refusent les certificats
auto-signés : l'application démarre un **relais local** (`relay/`, Rust) sur `127.0.0.1` qui
transmet tout au serveur en HTTPS avec le **certificat épinglé** (empreinte validée au premier
lancement, connexion bloquée s'il change). L'écran local (`ui/`) reprend la carte de connexion
de l'interface web ; il suit le thème clair/sombre du système.
