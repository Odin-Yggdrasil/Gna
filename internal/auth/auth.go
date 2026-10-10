// Package auth contient la logique d'authentification : mot de passe, puis TOTP obligatoire.
//
// Parcours d'une connexion :
//
//	login (mot de passe OK) ──► session "mfa"    ──► code TOTP ──► session "full"
//	                       └──► session "enroll" ──► QR + code ──► session "full"  (premier login)
//
// Les sessions intermédiaires durent 5 minutes et ne donnent accès à rien d'autre.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gna/internal/config"
	"gna/internal/secure"
	"gna/internal/store"
	"gna/internal/totp"
)

const (
	StageMFA    = "mfa"
	StageEnroll = "enroll"
	StageFull   = "full"

	preAuthTTL = 5 * time.Minute
)

var (
	ErrInvalidCredentials = errors.New("identifiants invalides")
	ErrInvalidCode        = errors.New("code invalide")
	ErrUnauthenticated    = errors.New("non authentifié")
	ErrWrongStage         = errors.New("étape d'authentification incorrecte")
)

type Service struct {
	st  *store.Store
	cfg *config.Config
	box *secure.Box
	now func() time.Time
}

func New(st *store.Store, cfg *config.Config, box *secure.Box) *Service {
	return &Service{st: st, cfg: cfg, box: box, now: time.Now}
}

// Client décrit l'origine d'une requête, pour l'audit.
type Client struct{ IP, UserAgent string }

// Identity est le résultat d'une authentification de requête.
type Identity struct {
	Session *store.Session
	User    *store.User
	Token   string
}

// ---------- Jetons ----------

func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// ---------- Connexion ----------

// Login vérifie le mot de passe et ouvre une session intermédiaire.
// Le message d'erreur est identique que le compte existe ou non (pas d'énumération).
func (s *Service) Login(ctx context.Context, username, password string, c Client) (token, stage string, err error) {
	u, err := s.st.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		secure.BurnTime(password)
		s.audit(ctx, nil, username, "login.failed", c, map[string]any{"reason": "unknown_user"})
		return "", "", ErrInvalidCredentials
	}
	if err != nil {
		return "", "", err
	}

	ok, err := secure.VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return "", "", err
	}
	switch {
	case u.Disabled:
		s.audit(ctx, &u.ID, u.Username, "login.failed", c, map[string]any{"reason": "disabled"})
		return "", "", ErrInvalidCredentials
	case u.LockedUntil != nil && u.LockedUntil.After(s.now()):
		s.audit(ctx, &u.ID, u.Username, "login.failed", c, map[string]any{"reason": "locked"})
		return "", "", ErrInvalidCredentials
	case !ok:
		s.failure(ctx, u, c, "bad_password")
		return "", "", ErrInvalidCredentials
	}

	stage = StageMFA
	if !u.TOTPEnabled {
		stage = StageEnroll
	}
	token, hash, err := newToken()
	if err != nil {
		return "", "", err
	}
	if err := s.st.CreateSession(ctx, hash, u.ID, stage, s.now().Add(preAuthTTL), c.IP, c.UserAgent); err != nil {
		return "", "", err
	}
	s.audit(ctx, &u.ID, u.Username, "login.password_ok", c, map[string]any{"next": stage})
	return token, stage, nil
}

// VerifyTOTP valide le second facteur et transforme la session en session complète.
// Renvoie le nouveau jeton (l'ancien est détruit).
func (s *Service) VerifyTOTP(ctx context.Context, id *Identity, code string, c Client) (string, error) {
	if id.Session.Stage != StageMFA {
		return "", ErrWrongStage
	}
	u := id.User
	secret, err := s.box.Open(u.TOTPSecret, totpContext(u.ID))
	if err != nil {
		return "", fmt.Errorf("déchiffrement du secret TOTP : %w", err)
	}
	step, ok := totp.Validate(secret, code, s.now(), u.TOTPLastStep)
	if ok {
		ok, err = s.st.ConsumeTOTPStep(ctx, u.ID, step)
		if err != nil {
			return "", err
		}
	}
	if !ok {
		s.failure(ctx, u, c, "bad_totp")
		return "", ErrInvalidCode
	}
	return s.promote(ctx, id, c, "login.success")
}

// StartEnrollment génère un nouveau secret TOTP et renvoie l'URI otpauth://
// (pour le QR code) ainsi que le secret en clair (pour une saisie manuelle).
func (s *Service) StartEnrollment(ctx context.Context, id *Identity) (uri, secretB32 string, err error) {
	if id.Session.Stage != StageEnroll {
		return "", "", ErrWrongStage
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		return "", "", err
	}
	sealed, err := s.box.Seal(secret, totpContext(id.User.ID))
	if err != nil {
		return "", "", err
	}
	if err := s.st.SetPendingTOTP(ctx, id.User.ID, sealed); err != nil {
		return "", "", err
	}
	return totp.URI(s.cfg.Issuer, id.User.Username, secret), totp.EncodeSecret(secret), nil
}

// ConfirmEnrollment active la MFA si le code saisi correspond au secret en attente.
func (s *Service) ConfirmEnrollment(ctx context.Context, id *Identity, code string, c Client) (string, error) {
	if id.Session.Stage != StageEnroll {
		return "", ErrWrongStage
	}
	// Relire l'utilisateur : le secret a pu être (re)généré après l'ouverture de la requête.
	u, err := s.st.UserByID(ctx, id.User.ID)
	if err != nil {
		return "", err
	}
	if len(u.TOTPSecret) == 0 {
		return "", ErrInvalidCode
	}
	secret, err := s.box.Open(u.TOTPSecret, totpContext(u.ID))
	if err != nil {
		return "", fmt.Errorf("déchiffrement du secret TOTP : %w", err)
	}
	step, ok := totp.Validate(secret, code, s.now(), 0)
	if !ok {
		s.failure(ctx, u, c, "bad_totp_enroll")
		return "", ErrInvalidCode
	}
	if err := s.st.EnableTOTP(ctx, u.ID, step); err != nil {
		return "", err
	}
	s.audit(ctx, &u.ID, u.Username, "mfa.enrolled", c, nil)
	return s.promote(ctx, &Identity{Session: id.Session, User: u, Token: id.Token}, c, "login.success")
}

func (s *Service) promote(ctx context.Context, id *Identity, c Client, event string) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.st.RotateSession(ctx, id.Session.TokenHash, hash, StageFull, s.now().Add(s.cfg.SessionTTL)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrUnauthenticated
		}
		return "", err
	}
	if err := s.st.ResetLoginFailures(ctx, id.User.ID); err != nil {
		return "", err
	}
	s.audit(ctx, &id.User.ID, id.User.Username, event, c, nil)
	return token, nil
}

func (s *Service) Logout(ctx context.Context, id *Identity, c Client) error {
	s.audit(ctx, &id.User.ID, id.User.Username, "logout", c, nil)
	return s.st.DeleteSession(ctx, id.Session.TokenHash)
}

// Authenticate retrouve la session associée à un jeton. Elle applique l'expiration
// absolue et le délai d'inactivité, et vérifie que le compte est toujours actif.
func (s *Service) Authenticate(ctx context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	hash := HashToken(token)
	se, err := s.st.SessionByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if se.Stage == StageFull && now.Sub(se.LastSeenAt) > s.cfg.IdleTimeout {
		_ = s.st.DeleteSession(ctx, hash)
		return nil, ErrUnauthenticated
	}
	u, err := s.st.UserByID(ctx, se.UserID)
	if err != nil || u.Disabled {
		_ = s.st.DeleteSession(ctx, hash)
		return nil, ErrUnauthenticated
	}
	// Limiter les écritures : on ne met à jour last_seen qu'une fois par minute.
	if now.Sub(se.LastSeenAt) > time.Minute {
		_ = s.st.TouchSession(ctx, hash)
	}
	return &Identity{Session: se, User: u, Token: token}, nil
}

// Revalidate revérifie périodiquement une session pendant une connexion longue (terminal).
// active indique s'il y a eu une activité de l'utilisateur depuis la dernière vérification :
// seule l'activité réelle repousse le délai d'inactivité.
// Renvoie l'utilisateur relu en base (ses droits ont pu changer entre-temps).
func (s *Service) Revalidate(ctx context.Context, token string, active bool) (*store.User, error) {
	hash := HashToken(token)
	se, err := s.st.SessionByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if !active && s.now().Sub(se.LastSeenAt) > s.cfg.IdleTimeout {
		_ = s.st.DeleteSession(ctx, hash)
		return nil, ErrUnauthenticated
	}
	u, err := s.st.UserByID(ctx, se.UserID)
	if err != nil || u.Disabled {
		return nil, ErrUnauthenticated
	}
	if active {
		return u, s.st.TouchSession(ctx, hash)
	}
	return u, nil
}

// Box donne accès au chiffrement des secrets (identifiants des cibles).
func (s *Service) Box() *secure.Box { return s.box }

// ---------- Utilitaires ----------

func (s *Service) failure(ctx context.Context, u *store.User, c Client, reason string) {
	locked, err := s.st.RecordLoginFailure(ctx, u.ID, s.cfg.MaxFailures, s.cfg.LockDuration)
	if err != nil {
		slog.Error("enregistrement d'un échec de connexion", "err", err)
	}
	s.audit(ctx, &u.ID, u.Username, "login.failed", c, map[string]any{"reason": reason})
	if locked {
		s.audit(ctx, &u.ID, u.Username, "account.locked", c, map[string]any{"duration": s.cfg.LockDuration.String()})
	}
}

func (s *Service) audit(ctx context.Context, uid *int64, username, event string, c Client, details map[string]any) {
	if err := s.st.Audit(ctx, uid, username, event, c.IP, details); err != nil {
		slog.Error("écriture du journal d'audit", "event", event, "err", err)
	}
}

func totpContext(userID int64) string { return fmt.Sprintf("totp:%d", userID) }
