-- Gná : schéma initial (utilisateurs, sessions, journal d'audit)

CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    username        TEXT        NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9._-]{2,64}$'),
    password_hash   TEXT        NOT NULL,
    is_admin        BOOLEAN     NOT NULL DEFAULT FALSE,
    disabled        BOOLEAN     NOT NULL DEFAULT FALSE,
    totp_secret     BYTEA,                          -- chiffré (AES-256-GCM, clé maître)
    totp_enabled    BOOLEAN     NOT NULL DEFAULT FALSE,
    totp_last_step  BIGINT      NOT NULL DEFAULT 0, -- anti-rejeu des codes TOTP
    failed_logins   INT         NOT NULL DEFAULT 0,
    locked_until    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seul le hash SHA-256 du jeton est stocké : une fuite de la base ne donne pas de session valide.
CREATE TABLE sessions (
    token_hash   BYTEA       PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    stage        TEXT        NOT NULL CHECK (stage IN ('mfa', 'enroll', 'full')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    ip           TEXT        NOT NULL DEFAULT '',
    user_agent   TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user_idx    ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE TABLE audit_log (
    id        BIGSERIAL   PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id   BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    username  TEXT        NOT NULL DEFAULT '',
    event     TEXT        NOT NULL,
    ip        TEXT        NOT NULL DEFAULT '',
    details   JSONB       NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- Le journal d'audit est en ajout seul : ni modification ni suppression.
-- (Exception : la mise à NULL de user_id quand un compte est supprimé.)
CREATE FUNCTION audit_log_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.id = OLD.id AND NEW.at = OLD.at AND NEW.username = OLD.username
       AND NEW.event = OLD.event AND NEW.ip = OLD.ip AND NEW.details = OLD.details
       AND NEW.user_id IS NULL THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log est en ajout seul';
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_guard
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_guard();
