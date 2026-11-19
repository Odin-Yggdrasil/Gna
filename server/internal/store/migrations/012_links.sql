-- Gná : liaisons Runa. Un autre outil de la famille (Völund) ajoute ou met à jour des
-- machines avec un jeton de service limité à des portées. Seule l'empreinte SHA-256 du
-- jeton est stockée. Une liaison jamais utilisée expire à pending_until.
CREATE TABLE IF NOT EXISTS links (
    id            BIGSERIAL   PRIMARY KEY,
    name          TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    token_hash    BYTEA       NOT NULL UNIQUE,
    scopes        TEXT[]      NOT NULL,
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    pending_until TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    last_used_ip  TEXT        NOT NULL DEFAULT ''
);

-- Machine créée par une liaison : sa référence chez l'outil lié (ex. « machine:12 »),
-- pour la retrouver à la réinstallation suivante au lieu d'en créer une autre.
ALTER TABLE targets ADD COLUMN IF NOT EXISTS link_id  BIGINT REFERENCES links(id) ON DELETE SET NULL;
ALTER TABLE targets ADD COLUMN IF NOT EXISTS link_ref TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS targets_link_ref_idx ON targets (link_id, link_ref) WHERE link_id IS NOT NULL;
