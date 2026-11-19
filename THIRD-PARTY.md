# Composants tiers

Gná est un logiciel libre, publié sous licence GNU Affero General Public License v3.0 ou
ultérieure (voir `LICENSE`). Il inclut ou utilise les composants suivants, qui restent
soumis à leur propre licence :

| Composant | Emplacement | Licence |
|---|---|---|
| noVNC | `server/internal/web/static/vendor/novnc/` | MPL-2.0 (fichiers à garder sous MPL, non modifiés) |
| pako (via noVNC) | `server/internal/web/static/vendor/novnc/vendor/pako/` | MIT |
| xterm.js | `server/internal/web/static/vendor/` | MIT |
| asciinema-player | `server/internal/web/static/vendor/asciinema/` | Apache-2.0 |
| ironrdp-rdpdr (version modifiée) | `server/rdpbridge/vendor/ironrdp-rdpdr/` | MIT ou Apache-2.0 |
| Dépendances Go, Rust et npm | `server/go.mod`, `Cargo.toml`, `package.json` | voir chaque module (MIT, BSD, Apache-2.0, ISC…) |

Toutes ces licences sont compatibles avec l'AGPL-3.0 : leurs mentions de copyright et de
licence doivent être conservées dans les copies et les versions modifiées.
