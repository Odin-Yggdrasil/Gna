// Package store gère l'accès à PostgreSQL.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound  = errors.New("introuvable")
	ErrDuplicate = errors.New("existe déjà")
)

type Store struct{ db *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connexion à PostgreSQL impossible : %w", err)
	}
	return &Store{db: pool}, nil
}

func (s *Store) Close() { s.db.Close() }

// Migrate applique les migrations embarquées qui ne l'ont pas encore été.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		name := strings.TrimPrefix(f, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("nom de migration invalide : %s", name)
		}
		sql, _ := migrationsFS.ReadFile(f)
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			// Verrou pour éviter deux instances qui migrent en même temps.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7319)`); err != nil {
				return err
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&done); err != nil || done {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s : %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------- Utilisateurs ----------

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	Disabled     bool
	TOTPSecret   []byte
	TOTPEnabled  bool
	TOTPLastStep int64
	FailedLogins int
	LockedUntil  *time.Time
	CreatedAt    time.Time
}

const userCols = `id, username, password_hash, is_admin, disabled, totp_secret, totp_enabled,
	totp_last_step, failed_logins, locked_until, created_at`

func scanUser(row pgx.Row) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &u.TOTPSecret,
		&u.TOTPEnabled, &u.TOTPLastStep, &u.FailedLogins, &u.LockedUntil, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, admin bool) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO users (username, password_hash, is_admin) VALUES ($1,$2,$3) RETURNING id`,
		username, passwordHash, admin).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, ErrDuplicate
	}
	return id, err
}

func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username=$1`, username))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecordLoginFailure incrémente le compteur d'échecs et verrouille le compte
// une fois le seuil atteint. Renvoie true si le compte vient d'être verrouillé.
func (s *Store) RecordLoginFailure(ctx context.Context, id int64, max int, lock time.Duration) (bool, error) {
	var locked bool
	err := s.db.QueryRow(ctx, `
		UPDATE users SET
			failed_logins = CASE WHEN failed_logins + 1 >= $2 THEN 0 ELSE failed_logins + 1 END,
			locked_until  = CASE WHEN failed_logins + 1 >= $2 THEN now() + make_interval(secs => $3) ELSE locked_until END,
			updated_at = now()
		WHERE id = $1
		RETURNING failed_logins = 0`, id, max, lock.Seconds()).Scan(&locked)
	return locked, err
}

func (s *Store) ResetLoginFailures(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET failed_logins=0, locked_until=NULL, updated_at=now() WHERE id=$1`, id)
	return err
}

// SetPendingTOTP enregistre un nouveau secret (pas encore actif) pendant l'enrôlement.
func (s *Store) SetPendingTOTP(ctx context.Context, id int64, sealed []byte) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_secret=$2, totp_enabled=FALSE, totp_last_step=0, updated_at=now()
		WHERE id=$1`, id, sealed)
	return err
}

func (s *Store) EnableTOTP(ctx context.Context, id int64, step int64) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET totp_enabled=TRUE, totp_last_step=$2, updated_at=now()
		WHERE id=$1 AND totp_secret IS NOT NULL`, id, step)
	return err
}

// ConsumeTOTPStep enregistre le pas utilisé de façon atomique. Renvoie false si
// un code de ce pas (ou plus récent) a déjà été accepté entre-temps : rejeu.
func (s *Store) ConsumeTOTPStep(ctx context.Context, id, step int64) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE users SET totp_last_step=$2 WHERE id=$1 AND totp_last_step < $2`, id, step)
	return tag.RowsAffected() == 1, err
}

// ResetTOTP désactive la MFA du compte et ferme ses sessions : il devra se réenrôler.
func (s *Store) ResetTOTP(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET totp_secret=NULL, totp_enabled=FALSE, totp_last_step=0,
			updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
		return err
	})
}

func (s *Store) SetPassword(ctx context.Context, id int64, hash string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, failed_logins=0, locked_until=NULL,
			updated_at=now() WHERE id=$1`, id, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
		return err
	})
}

func (s *Store) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET disabled=$2, updated_at=now() WHERE id=$1`, id, disabled); err != nil {
			return err
		}
		if !disabled {
			return nil
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
		return err
	})
}

// ---------- Sessions ----------

type Session struct {
	TokenHash  []byte
	UserID     int64
	Stage      string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int64, stage string, expires time.Time, ip, ua string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, stage, expires_at, ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6)`, tokenHash, userID, stage, expires, ip, truncate(ua, 300))
	return err
}

// SessionByHash renvoie une session non expirée.
func (s *Store) SessionByHash(ctx context.Context, tokenHash []byte) (*Session, error) {
	se := &Session{}
	err := s.db.QueryRow(ctx, `SELECT token_hash, user_id, stage, created_at, last_seen_at, expires_at
		FROM sessions WHERE token_hash=$1 AND expires_at > now()`, tokenHash).
		Scan(&se.TokenHash, &se.UserID, &se.Stage, &se.CreatedAt, &se.LastSeenAt, &se.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return se, err
}

func (s *Store) TouchSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.Exec(ctx, `UPDATE sessions SET last_seen_at=now() WHERE token_hash=$1`, tokenHash)
	return err
}

// RotateSession remplace un jeton par un nouveau en changeant d'étape
// (ex. "mfa" → "full"). Changer de jeton à chaque élévation empêche la fixation de session.
func (s *Store) RotateSession(ctx context.Context, oldHash, newHash []byte, stage string, expires time.Time) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var uid int64
		var ip, ua string
		err := tx.QueryRow(ctx, `DELETE FROM sessions WHERE token_hash=$1 AND expires_at > now()
			RETURNING user_id, ip, user_agent`, oldHash).Scan(&uid, &ip, &ua)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, stage, expires_at, ip, user_agent)
			VALUES ($1,$2,$3,$4,$5,$6)`, newHash, uid, stage, expires, ip, ua)
		return err
	})
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context, idle time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now() OR last_seen_at < now() - make_interval(secs => $1)`, idle.Seconds())
	return tag.RowsAffected(), err
}

// ---------- Audit ----------

type AuditEntry struct {
	ID       int64          `json:"id"`
	At       time.Time      `json:"at"`
	UserID   *int64         `json:"user_id,omitempty"`
	Username string         `json:"username"`
	Event    string         `json:"event"`
	IP       string         `json:"ip"`
	Details  map[string]any `json:"details"`
}

func (s *Store) Audit(ctx context.Context, userID *int64, username, event, ip string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := s.db.Exec(ctx, `INSERT INTO audit_log (user_id, username, event, ip, details) VALUES ($1,$2,$3,$4,$5)`,
		userID, truncate(username, 64), event, ip, details)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT id, at, user_id, username, event, ip, details
		FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.Username, &e.Event, &e.IP, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
