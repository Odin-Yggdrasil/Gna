-- Gná : cibles RDP (via la passerelle gna-rdp)
ALTER TABLE targets DROP CONSTRAINT targets_protocol_check;
ALTER TABLE targets ADD CONSTRAINT targets_protocol_check CHECK (protocol IN ('ssh', 'rdp', 'vnc'));
-- Disposition du clavier de la cible RDP (fr, fr-linux, us). host_key contient l'empreinte
-- SHA-256 du certificat RDP épinglé.
ALTER TABLE targets ADD COLUMN keyboard_layout TEXT NOT NULL DEFAULT '';
