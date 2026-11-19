#!/usr/bin/env python3
"""Met à jour le site de Gná (dossier site/) avec les derniers fichiers d'installation.

Contrairement à Saga, Andvari et Völund, les pages de Gná sont écrites à la main : ce script
ne régénère pas la documentation, il y reporte seulement les versions et les empreintes.

- telechargements/ : dernier fichier de chaque sorte (serveur, application Linux .deb et
  AppImage, Windows, Android) pris dans clients/installateurs/, et SHA256SUMS. Ouvert par
  défaut, comme jusqu'ici ; GNA_SITE_TELECHARGEMENT=0 le ferme (dossier vidé, « bientôt
  disponible » sur la page) ;
- index.html : bloc de téléchargement (nom, taille, SHA-256) et numéros de version ;
- docs.html : tableau des empreintes, noms des fichiers et numéros de version ;
- vérifie que tous les liens internes (#ancre) des pages existent.

Usage : python3 -I scripts/build-site.py
"""
import glob
import hashlib
import os
import re
import shutil
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SITE = os.path.join(ROOT, "site")
INST = os.path.join(ROOT, "clients", "installateurs")
TELECHARGEMENT = os.environ.get("GNA_SITE_TELECHARGEMENT", "1") != "0"

# (sorte, motif du fichier, système, détail, motif de remplacement dans les pages)
KINDS = [
    ("server", "gna-server_*_amd64.deb", "Serveur", "Debian 12+ / Ubuntu 22.04+ · amd64", r"gna-server_\d+\.\d+\.\d+_amd64\.deb"),
    ("windows", "Gna-*-windows-setup.exe", "Windows", "Windows 10 / 11 · installeur", r"Gna-\d+\.\d+\.\d+-windows-setup\.exe"),
    ("android", "Gna-*-android.apk", "Android", "Android 7+", r"Gna-\d+\.\d+\.\d+-android\.apk"),
    ("linux-deb", "gna-app_*_amd64.deb", "Linux", "Debian / Ubuntu · amd64", r"gna-app_\d+\.\d+\.\d+_amd64\.deb"),
    ("appimage", "Gna-*-x86_64.AppImage", "Linux", "Autres distributions · x86-64", r"Gna-\d+\.\d+\.\d+-x86_64\.AppImage"),
]


def vkey(path):
    v = re.search(r"[_-](\d+)\.(\d+)\.(\d+)[_-]", os.path.basename(path))
    return tuple(int(x) for x in v.groups())


def latest(pattern):
    files = glob.glob(os.path.join(INST, pattern))
    if not files:
        sys.exit("aucun fichier %s dans clients/installateurs/" % pattern)
    return max(files, key=vkey)


def human(n):
    mo = n / 1048576
    return (("%.1f" % mo).replace(".", ",") if mo < 10 else "%d" % round(mo)) + " Mo"


def main():
    pkgs = []
    for kind, pattern, os_, meta, rx in KINDS:
        path = latest(pattern)
        data = open(path, "rb").read()
        pkgs.append(dict(kind=kind, path=path, name=os.path.basename(path), size=len(data),
                         sha=hashlib.sha256(data).hexdigest(), os=os_, meta=meta, rx=rx))
    server = pkgs[0]
    version = re.search(r"_(\d+\.\d+\.\d+)_", server["name"]).group(1)
    expected = open(os.path.join(ROOT, "VERSION")).read().strip()
    if version != expected:
        sys.exit("le dernier paquet serveur est en %s mais VERSION indique %s : lancez make deb" % (version, expected))

    # Téléchargements
    dl = os.path.join(SITE, "telechargements")
    os.makedirs(dl, exist_ok=True)
    for old in os.listdir(dl):
        os.remove(os.path.join(dl, old))
    if TELECHARGEMENT:
        with open(os.path.join(dl, "SHA256SUMS"), "w") as f:
            for p in pkgs:
                shutil.copy2(p["path"], dl)
                f.write("%s  %s\n" % (p["sha"], p["name"]))

    def common(s):
        for p in pkgs:
            s = re.sub(p["rx"], p["name"], s)
        s = re.sub(r"(<(?:b|small|span) data-version>)v[\d.]+", r"\g<1>v" + version, s)
        return s

    # Page d'accueil
    blocs = []
    for p in pkgs:
        main_cls = " dl-main" if p["kind"] == "server" else ""
        if TELECHARGEMENT:
            blocs.append('''<a class="dl%s" href="telechargements/%s" download>
            <span class="dl-os">%s</span>
            <span class="dl-file">%s</span>
            <span class="dl-meta">%s · %s<span class="hash">SHA-256 %s</span></span>
          </a>''' % (main_cls, p["name"], p["os"], p["name"], p["meta"], human(p["size"]), p["sha"]))
        else:
            blocs.append('''<div class="dl dl-off%s">
            <span class="dl-os">%s</span>
            <span class="dl-file">%s</span>
            <span class="dl-meta">%s · bientôt disponible</span>
          </div>''' % (main_cls, p["os"], p["name"], p["meta"]))
    sums = '<a href="telechargements/SHA256SUMS" download>SHA256SUMS</a> · ' if TELECHARGEMENT else ""
    bloc = ("\n          ".join(blocs) + '\n          <p class="dl-sums">%s<a href="docs.html#verifier">Vérifier un téléchargement</a>'
            ' · <a href="docs.html#installer-app">Installer l\'application</a></p>' % sums)
    path = os.path.join(SITE, "index.html")
    s = open(path, encoding="utf-8").read()
    s, n = re.subn(r"(<!-- telechargement:fichier:debut -->\n\s*).*?(\n\s*<!-- telechargement:fichier:fin -->)",
                   lambda m: m.group(1) + bloc + m.group(2), s, flags=re.S)
    if n != 1:
        sys.exit("index.html : repères telechargement:fichier introuvables")
    open(path, "w", encoding="utf-8").write(common(s))

    # Documentation : empreintes
    rows = "\n          ".join('<tr><td><code>%s</code></td><td><code>%s</code></td></tr>' % (p["name"], p["sha"]) for p in pkgs)
    path = os.path.join(SITE, "docs.html")
    s = open(path, encoding="utf-8").read()
    s, n = re.subn(r"(<!-- empreintes:debut -->\n\s*).*?\s*(<!-- empreintes:fin -->)",
                   lambda m: m.group(1) + rows + "\n          " + m.group(2), s, flags=re.S)
    if n != 1:
        sys.exit("docs.html : repères empreintes introuvables")
    open(path, "w", encoding="utf-8").write(common(s))

    path = os.path.join(SITE, "mentions-legales.html")
    if os.path.exists(path):
        s = common(open(path, encoding="utf-8").read())
        open(path, "w", encoding="utf-8").write(s)

    # Vérification des ancres
    pages = [p for p in ("index.html", "docs.html", "mentions-legales.html") if os.path.exists(os.path.join(SITE, p))]
    ids = {p: set(re.findall(r'id="([^"]+)"', open(os.path.join(SITE, p), encoding="utf-8").read())) for p in pages}
    bad = []
    for p in pages:
        s = open(os.path.join(SITE, p), encoding="utf-8").read()
        for target, frag in re.findall(r'href="([^"#:]*)#([^"]+)"', s):
            target = target or p
            if target in ids and frag not in ids[target]:
                bad.append("%s → %s#%s" % (p, target, frag))
    if bad:
        sys.exit("ancres introuvables :\n  " + "\n  ".join(bad))

    print("site construit : Gná %s, téléchargement %s" % (version, "ouvert" if TELECHARGEMENT else "fermé"))
    for p in pkgs:
        print("  %-9s %s (%s)" % (p["kind"], p["name"], human(p["size"])))


if __name__ == "__main__":
    main()
