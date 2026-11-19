package store

// Jetons d'accès personnels : seule leur empreinte est stockée, jamais le jeton.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxAPITokens limite le nombre de jetons d'un utilisateur.
const MaxAPITokens = 20

type APIToken struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"-"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `json:"last_used_ip"`
}

func (s *Store) APITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT id, user_id, name, created_at, last_used_at, last_used_ip
		FROM api_tokens WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.LastUsedIP); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateAPIToken enregistre un jeton (empreinte uniquement). Erreur ErrLimit au-delà de MaxAPITokens.
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name string, hash []byte) (*APIToken, error) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE user_id=$1`, userID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= MaxAPITokens {
		return nil, ErrLimit
	}
	t := &APIToken{UserID: userID, Name: name}
	err := s.db.QueryRow(ctx, `INSERT INTO api_tokens (user_id, name, token_hash) VALUES ($1,$2,$3) RETURNING id, created_at`,
		userID, name, hash).Scan(&t.ID, &t.CreatedAt)
	return t, err
}

// DeleteAPIToken révoque un jeton de l'utilisateur (ErrNotFound s'il ne lui appartient pas).
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id int64) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `DELETE FROM api_tokens WHERE id=$1 AND user_id=$2 RETURNING name`, id, userID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// APITokenByHash retrouve un jeton à partir de son empreinte.
func (s *Store) APITokenByHash(ctx context.Context, hash []byte) (*APIToken, error) {
	t := &APIToken{}
	err := s.db.QueryRow(ctx, `SELECT id, user_id, name, created_at, last_used_at, last_used_ip
		FROM api_tokens WHERE token_hash=$1`, hash).Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.LastUsedIP)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (s *Store) TouchAPIToken(ctx context.Context, id int64, ip string) error {
	_, err := s.db.Exec(ctx, `UPDATE api_tokens SET last_used_at=now(), last_used_ip=$2 WHERE id=$1`, id, ip)
	return err
}
