-- Gná : machines cibles et historique des sessions d'accès

CREATE TABLE targets (
    id          BIGSERIAL   PRIMARY KEY,
    name        TEXT        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 64),
    protocol    TEXT        NOT NULL CHECK (protocol IN ('ssh', 'rdp', 'vnc')),
    host        TEXT        NOT NULL CHECK (length(host) BETWEEN 1 AND 255),
    port        INT         NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username    TEXT        NOT NULL DEFAULT '',
    auth_method TEXT        NOT NULL DEFAULT 'password' CHECK (auth_method IN ('password', 'key')),
    secret      BYTEA,      -- mot de passe ou clé privée, chiffré (AES-256-GCM, clé maître)
    passphrase  BYTEA,      -- phrase de passe de la clé privée, chiffrée
    host_key    TEXT        NOT NULL DEFAULT '', -- clé d'hôte SSH épinglée (format authorized_keys)
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Une ligne par connexion ouverte vers une cible (servira aussi aux enregistrements).
CREATE TABLE access_sessions (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    username    TEXT        NOT NULL,
    target_id   BIGINT      REFERENCES targets(id) ON DELETE SET NULL,
    target_name TEXT        NOT NULL,
    protocol    TEXT        NOT NULL,
    client_ip   TEXT        NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ,
    end_reason  TEXT        NOT NULL DEFAULT '',
    bytes_in    BIGINT      NOT NULL DEFAULT 0,
    bytes_out   BIGINT      NOT NULL DEFAULT 0
);
CREATE INDEX access_sessions_started_idx ON access_sessions (started_at DESC);

-- Au redémarrage de Gná, les sessions restées ouvertes sont closes (voir store.CloseDanglingAccessSessions).
