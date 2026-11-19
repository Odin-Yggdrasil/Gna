-- Gná : dossiers et étiquettes fusionnés en un seul classement, le dossier.
-- Une machine sans dossier prend sa première étiquette comme dossier.
UPDATE targets SET folder = left(tags[1], 64) WHERE folder = '' AND cardinality(tags) > 0;
ALTER TABLE targets DROP COLUMN tags;
