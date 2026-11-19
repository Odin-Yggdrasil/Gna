-- Gná : organisation des machines, bibliothèque de commandes, espaces de travail.

-- Dossier et étiquettes des cibles (définis par un administrateur).
ALTER TABLE targets ADD COLUMN folder TEXT NOT NULL DEFAULT '' CHECK (length(folder) <= 64);
ALTER TABLE targets ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';

-- Favoris : propres à chaque utilisateur.
CREATE TABLE target_favorites (
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_id BIGINT NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, target_id)
);

-- Bibliothèque de commandes personnelle.
CREATE TABLE snippets (
    id         BIGSERIAL   PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    command    TEXT        NOT NULL CHECK (length(command) BETWEEN 1 AND 8000),
    run        BOOLEAN     NOT NULL DEFAULT true,  -- validée (Entrée) à l'envoi
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

-- Espaces de travail : un ensemble de sessions et leur disposition.
CREATE TABLE workspaces (
    id         BIGSERIAL   PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    layout     TEXT        NOT NULL DEFAULT 'tabs' CHECK (layout IN ('tabs', 'grid')),
    sync       BOOLEAN     NOT NULL DEFAULT false,
    items      JSONB       NOT NULL DEFAULT '[]',  -- [{"target_id": 1, "sync": true}, …]
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
