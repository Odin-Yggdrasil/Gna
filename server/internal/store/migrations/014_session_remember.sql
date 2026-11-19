-- Gná : « Rester connecté sur cet appareil ». Une telle session n'est pas fermée après
-- inactivité ; elle reste limitée par sa durée maximale (GNA_SESSION_TTL).
ALTER TABLE sessions ADD COLUMN remember BOOLEAN NOT NULL DEFAULT FALSE;
