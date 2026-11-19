-- Gná : verrouillage des sessions inactives.
-- Après inactivité, une session « full » passe à l'état « locked » : le mot de passe seul
-- la déverrouille tant que le dernier code TOTP (mfa_at) est assez récent (GNA_MFA_REMEMBER).
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_stage_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_stage_check CHECK (stage IN ('mfa', 'enroll', 'full', 'locked'));
ALTER TABLE sessions ADD COLUMN mfa_at TIMESTAMPTZ;
UPDATE sessions SET mfa_at = created_at WHERE stage = 'full';
