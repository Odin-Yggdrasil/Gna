# Dépendances modifiées

`ironrdp-rdpdr` 0.7.0, avec une seule modification (`src/pdu/efs.rs`, `new_drive`) :
le nom court du lecteur (PreferredDosName) reçoit le nom réel au lieu de « ignored ».
xrdp affiche ce nom court (`~/thinclient_drives/Gna`) ; Windows utilise le nom complet.
