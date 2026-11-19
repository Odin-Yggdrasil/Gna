-- Gná : cibles VNC sans authentification possible
ALTER TABLE targets DROP CONSTRAINT targets_auth_method_check;
ALTER TABLE targets ADD CONSTRAINT targets_auth_method_check
    CHECK (auth_method IN ('password', 'key', 'none'));
