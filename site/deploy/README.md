# Publier le site de Gná

Le site est statique : il suffit de copier le dossier `site/` (sans ce dossier `deploy/`) sur un serveur web.

## Sécurité déjà intégrée aux pages

- Aucun script ni style inline : la Content-Security-Policy peut rester stricte.
- **Referrer-Policy `no-referrer`** : un clic vers un autre site ne révèle pas la page d'origine.
- Liens externes en `rel="noopener noreferrer"`.
- Seul le choix de thème (`light` ou `dark`) est stocké dans le navigateur, et la valeur relue est vérifiée.
- **Empreintes SHA-256** des paquets publiées sur la page Télécharger et dans `telechargements/SHA256SUMS`.

## Statistiques (Plausible)

Les deux pages chargent le script de Plausible, auto-hébergé sur `plausible.gjallarbru.ovh` (pas de cookie, pas de données envoyées à un tiers). C'est la seule ressource externe du site, et la CSP ne l'autorise qu'à cet endroit :

- `script-src 'self' https://plausible.gjallarbru.ovh` : charger le script ;
- `connect-src https://plausible.gjallarbru.ovh` : envoyer les visites.

Si l'adresse de Plausible change, il faut modifier à la fois la balise `<script>` des deux pages et la CSP (`nginx.conf` ou `_headers`).

## Ce que le serveur doit ajouter

La Content-Security-Policy n'est plus dans les pages : elle est envoyée **uniquement en en-tête HTTP**. Sans ces en-têtes, le site n'a pas de CSP. Les autres protections (HSTS, `frame-ancestors`, `X-Content-Type-Options`, `Permissions-Policy`…) ne fonctionnent de toute façon qu'en en-tête.

- `nginx.conf` : nginx du site, **derrière Nginx Proxy Manager** (voir ci-dessous).
- `_headers` : pour Cloudflare Pages, Netlify et hébergeurs compatibles, si le site est publié ailleurs.

## Mise en ligne derrière Nginx Proxy Manager

```
Visiteur ──HTTPS──► Nginx Proxy Manager ──HTTP──► nginx du site (/var/www/gna-web)
```

Partage des rôles :

| Où | Quoi |
|---|---|
| **NPM** | HTTPS, certificat Let's Encrypt, redirection HTTP → HTTPS, HTTP/2, HSTS |
| **nginx du site** | Fichiers, en-têtes de sécurité (CSP, nosniff, X-Frame-Options…), accès réservé à NPM |

### 1. nginx du site

Dans `nginx.conf`, adapter l'adresse IP de NPM (`allow`) et `server_name` si besoin, puis :

```bash
sudo mkdir -p /var/www/gna-web
sudo rsync -a --delete --exclude deploy/ ./site/ /var/www/gna-web/
sudo cp nginx.conf /etc/nginx/sites-available/gna-web
sudo ln -s /etc/nginx/sites-available/gna-web /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

Au pare-feu, n'ouvrir le port 80 de ce serveur qu'à NPM (ex. `sudo ufw allow from 10.0.66.1 to any port 80 proto tcp`). Le `allow`/`deny` de la config le fait déjà au niveau de nginx ; le pare-feu ajoute une deuxième barrière.

### 2. Proxy Host dans NPM

**Hosts → Proxy Hosts → Add Proxy Host**

- *Details* : nom de domaine, scheme `http`, adresse IP du serveur du site, port `80`. Cocher **Block Common Exploits**. *Websockets Support* est inutile pour ce site.
- *SSL* : **Request a new SSL Certificate** (Let's Encrypt), puis cocher **Force SSL**, **HTTP/2 Support**, **HSTS Enabled**. N'activer *HSTS Subdomains* que si **tous** les sous-domaines sont servis en HTTPS.
- *Advanced* : rien à ajouter. Les en-têtes viennent du nginx du site. Les redéclarer ici les enverrait en double.

### 3. Vérifier

```bash
curl -sI https://gna.gjallarbru.ovh/
```

Chaque en-tête doit apparaître **une seule fois** : `Strict-Transport-Security` (ajouté par NPM), `Content-Security-Policy`, `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, `Permissions-Policy`… Un `curl -sI http://IP-DU-SERVEUR-DU-SITE/` lancé depuis une autre machine que NPM doit répondre `403`.

## À chaque nouvelle version

Les fichiers viennent de la Release GitHub (dépôt privé `Odin-Yggdrasil/Gna`, donc inaccessible aux visiteurs : ils doivent être hébergés ici), sauf l'APK Android, compilé en local dans `clients/installateurs/` (`clients/app/scripts/build-android.sh`).

1. Remplacer les fichiers de `telechargements/` :
   ```bash
   cd telechargements && rm -f gna-server_* gna-app_* Gna-*
   gh release download v<version> -R Odin-Yggdrasil/Gna -p '*.deb' -p '*.exe' -p '*.AppImage' -p SHA256SUMS.txt
   sha256sum -c SHA256SUMS.txt && rm SHA256SUMS.txt
   cp ../../clients/installateurs/Gna-<version>-android.apk .
   ```
2. Régénérer les empreintes : `sha256sum gna-server_* gna-app_* Gna-* > SHA256SUMS`.
3. Mettre à jour les noms, tailles et empreintes dans `index.html` (page Télécharger) et `docs.html` (sections « Vérifier un téléchargement » et « Installer l'application »).
4. Si `style.css`, `script.js` ou `theme-init.js` ont changé, changer le numéro `?v=…` de leurs liens dans les trois pages (`index.html`, `docs.html`, `mentions-legales.html`). NPM met ces fichiers en cache (option « Cache Assets ») : sans nouveau numéro, les visiteurs garderaient l'ancienne version pendant quelques heures.

## À faire

- **security.txt** (RFC 9116) : publier `/.well-known/security.txt` pour indiquer comment signaler une faille. Il faut une adresse de contact valide, à ajouter quand elle sera choisie :

  ```
  Contact: mailto:securite@exemple.org
  Expires: 2027-10-11T00:00:00Z
  Preferred-Languages: fr, en
  Canonical: https://gna.example.org/.well-known/security.txt
  ```

- **Signature des paquets** : une empreinte prouve qu'un fichier n'a pas été altéré en route, mais pas qui l'a produit. Signer `SHA256SUMS` avec une clé GPG (`gpg --detached-sign --armor SHA256SUMS`) et publier la clé publique renforcerait la chaîne de confiance.
