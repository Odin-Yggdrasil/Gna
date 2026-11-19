-- Gná : mot de passe sudo d'une machine, reçu d'Andvari par une liaison Runa (chiffré
-- par la clé maître, comme les identifiants). Le bouton « sudo » du terminal le tape
-- dans la session, sans qu'il passe par le navigateur ni par l'enregistrement.
ALTER TABLE targets ADD COLUMN IF NOT EXISTS sudo_secret BYTEA;
