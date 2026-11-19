-- Gná : jetons d'accès personnels (scripts, menu du bureau). Lecture seule de la liste
-- des machines de l'utilisateur. Seule l'empreinte SHA-256 du jeton est stockée.
-- Numéro 011 et IF NOT EXISTS : certaines bases avaient déjà une version 10 enregistrée.
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGSERIAL   PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    token_hash   BYTEA       NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    last_used_ip TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS api_tokens_user_idx ON api_tokens (user_id);
