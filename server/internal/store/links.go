package store

// Liaisons Runa : jetons de service d'autres outils de la famille (Völund…), limités à
// des portées. Seule leur empreinte est stockée, jamais le jeton.

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Link struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Scopes       []string   `json:"scopes"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	PendingUntil *time.Time `json:"pending_until"` // nil : appairage confirmé
	LastUsedAt   *time.Time `json:"last_used_at"`
	LastUsedIP   string     `json:"last_used_ip"`
}

func (l *Link) Has(scope string) bool { return slices.Contains(l.Scopes, scope) }

const linkCols = `id, name, scopes, created_by, created_at, pending_until, last_used_at, last_used_ip`

func scanLink(row pgx.Row) (*Link, error) {
	l := &Link{}
	err := row.Scan(&l.ID, &l.Name, &l.Scopes, &l.CreatedBy, &l.CreatedAt, &l.PendingUntil, &l.LastUsedAt, &l.LastUsedIP)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

func (s *Store) CreateLink(ctx context.Context, name string, hash []byte, scopes []string, createdBy string, pendingUntil time.Time) (*Link, error) {
	return scanLink(s.db.QueryRow(ctx, `INSERT INTO links (name, token_hash, scopes, created_by, pending_until)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+linkCols, name, hash, scopes, createdBy, pendingUntil))
}

// Links renvoie les liaisons, après avoir effacé celles dont le code a expiré sans servir.
func (s *Store) Links(ctx context.Context) ([]*Link, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM links WHERE pending_until < now()`); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+linkCols+` FROM links ORDER BY lower(name), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Link{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UseLink renvoie la liaison d'un jeton valide et note son utilisation ; la première
// utilisation confirme l'appairage.
func (s *Store) UseLink(ctx context.Context, hash []byte, ip string) (*Link, error) {
	return scanLink(s.db.QueryRow(ctx, `UPDATE links SET pending_until = NULL, last_used_at = now(), last_used_ip = $2
		WHERE token_hash = $1 AND (pending_until IS NULL OR pending_until > now()) RETURNING `+linkCols, hash, ip))
}

func (s *Store) DeleteLink(ctx context.Context, id int64) (*Link, error) {
	return scanLink(s.db.QueryRow(ctx, `DELETE FROM links WHERE id=$1 RETURNING `+linkCols, id))
}

// LinkedTarget décrit une machine envoyée par une liaison. Seuls l'adresse, le port, la clé
// d'hôte et la description suivent l'outil lié : le nom, l'identifiant, les identifiants
// de connexion et les droits restent ceux réglés dans Gná.
type LinkedTarget struct {
	Ref         string
	Name        string
	Host        string
	Port        int
	Username    string
	HostKey     string
	Description string
	Folder      string
}

// UpsertLinkedTarget crée ou met à jour la machine (link, ref). Une machine d'une liaison
// révoquée puis recréée est reprise si elle porte la même référence et le même nom.
func (s *Store) UpsertLinkedTarget(ctx context.Context, linkID int64, in LinkedTarget) (id int64, created bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id FROM targets WHERE link_id=$1 AND link_ref=$2`, linkID, in.Ref).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `SELECT id FROM targets WHERE link_id IS NULL AND link_ref=$1 AND name=$2`, in.Ref, in.Name).Scan(&id)
		}
		switch {
		case err == nil:
			_, err = tx.Exec(ctx, `UPDATE targets SET host=$2, port=$3, host_key=$4, description=$5,
				link_id=$6, link_ref=$7, updated_at=now() WHERE id=$1`,
				id, in.Host, in.Port, in.HostKey, in.Description, linkID, in.Ref)
			return err
		case errors.Is(err, pgx.ErrNoRows):
			created = true
			// Sans identifiants : la machine est « à compléter » par un administrateur.
			return tx.QueryRow(ctx, `INSERT INTO targets (name, protocol, host, port, username, auth_method, host_key,
				description, keyboard_layout, folder, link_id, link_ref)
				VALUES ($1,'ssh',$2,$3,$4,'key',$5,$6,'',$7,$8,$9) RETURNING id`,
				in.Name, in.Host, in.Port, in.Username, in.HostKey, in.Description, in.Folder, linkID, in.Ref).Scan(&id)
		default:
			return err
		}
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, false, ErrDuplicate
	}
	return id, created, err
}

// TargetsByHost renvoie les machines SSH dont l'adresse (ou la référence de liaison
// « machine:<nom> ») correspond à host, sans tenir compte de la casse.
func (s *Store) TargetsByHost(ctx context.Context, host string) ([]*Target, error) {
	rows, err := s.db.Query(ctx, `SELECT `+targetCols+` FROM targets
		WHERE protocol = 'ssh' AND (lower(host) = lower($1) OR lower(link_ref) = lower('machine:' || $1)) ORDER BY id`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetSudoSecret range le mot de passe sudo (déjà chiffré) d'une machine.
func (s *Store) SetSudoSecret(ctx context.Context, id int64, sealed []byte) error {
	_, err := s.db.Exec(ctx, `UPDATE targets SET sudo_secret=$2, updated_at=now() WHERE id=$1`, id, sealed)
	return err
}

// CompleteTarget donne ses identifiants à une machine qui n'en avait pas (« à compléter ») :
// connexion par mot de passe. Sans effet si un secret a été saisi entre-temps.
func (s *Store) CompleteTarget(ctx context.Context, id int64, username string, sealed []byte) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE targets SET auth_method='password', username=$2, secret=$3, passphrase=NULL,
		updated_at=now() WHERE id=$1 AND secret IS NULL`, id, username, sealed)
	return err == nil && tag.RowsAffected() == 1, err
}
