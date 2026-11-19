-- Gná : enregistrements des sessions
ALTER TABLE access_sessions
    ADD COLUMN recording_path   TEXT    NOT NULL DEFAULT '',   -- fichier chiffré, vide si aucun
    ADD COLUMN recording_size   BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN recording_purged BOOLEAN NOT NULL DEFAULT FALSE; -- supprimé à l'expiration de la durée de conservation
