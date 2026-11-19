-- Gná : groupes et droits d'accès aux cibles

CREATE TABLE groups (
    id          BIGSERIAL   PRIMARY KEY,
    name        TEXT        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 64),
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_idx ON group_members (user_id);

-- Une ligne = un accès à une cible, accordé soit à un utilisateur, soit à un groupe.
-- Les administrateurs ont accès à toutes les cibles sans ligne ici.
CREATE TABLE target_access (
    id        BIGSERIAL PRIMARY KEY,
    target_id BIGINT NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    user_id   BIGINT REFERENCES users(id)  ON DELETE CASCADE,
    group_id  BIGINT REFERENCES groups(id) ON DELETE CASCADE,
    CHECK ((user_id IS NULL) <> (group_id IS NULL)),
    UNIQUE (target_id, user_id),
    UNIQUE (target_id, group_id)
);
CREATE INDEX target_access_user_idx  ON target_access (user_id);
CREATE INDEX target_access_group_idx ON target_access (group_id);
