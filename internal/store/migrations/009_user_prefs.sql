-- Gná : préférences d'affichage de chaque utilisateur (thème…), communes au navigateur
-- et à l'application.
ALTER TABLE users ADD COLUMN prefs JSONB NOT NULL DEFAULT '{}';
