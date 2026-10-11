# Composants tiers

Gná est un logiciel propriétaire et gratuit (voir `LICENSE`). Il inclut ou utilise les composants
suivants, qui restent soumis à leur propre licence :

| Composant | Emplacement | Licence |
|---|---|---|
| noVNC | `internal/web/static/vendor/novnc/` | MPL-2.0 (fichiers à garder sous MPL, non modifiés) |
| pako (via noVNC) | `internal/web/static/vendor/novnc/vendor/pako/` | MIT |
| xterm.js | `internal/web/static/vendor/` | MIT |
| asciinema-player | `internal/web/static/vendor/asciinema/` | Apache-2.0 |
| ironrdp-rdpdr (version modifiée) | `rdpbridge/vendor/ironrdp-rdpdr/` | MIT ou Apache-2.0 |
| Dépendances Go, Rust et npm | `go.mod`, `Cargo.toml`, `package.json` | voir chaque module (MIT, BSD, Apache-2.0, ISC…) |

Ces licences autorisent leur usage dans un logiciel commercial, à condition de
conserver leurs mentions de copyright et de licence.
