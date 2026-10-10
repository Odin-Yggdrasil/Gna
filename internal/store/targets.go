package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Target struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Protocol    string    `json:"protocol"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Username    string    `json:"username"`
	AuthMethod  string    `json:"auth_method"`
	HostKey     string    `json:"host_key"`
	Description string    `json:"description"`
	Layout      string    `json:"keyboard_layout"`
	Folder      string    `json:"folder"`
	HasSecret   bool      `json:"has_secret"`
	CreatedAt   time.Time `json:"created_at"`

	// Chiffrés, jamais renvoyés au navigateur.
	Secret     []byte `json:"-"`
	Passphrase []byte `json:"-"`
}

const targetCols = `id, name, protocol, host, port, username, auth_method, host_key, description,
	keyboard_layout, folder, secret IS NOT NULL, created_at, secret, passphrase`

func scanTarget(row pgx.Row) (*Target, error) {
	t := &Target{}
	err := row.Scan(&t.ID, &t.Name, &t.Protocol, &t.Host, &t.Port, &t.Username, &t.AuthMethod, &t.HostKey,
		&t.Description, &t.Layout, &t.Folder, &t.HasSecret, &t.CreatedAt, &t.Secret, &t.Passphrase)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (s *Store) ListTargets(ctx context.Context) ([]*Target, error) {
	rows, err := s.db.Query(ctx, `SELECT `+targetCols+` FROM targets ORDER BY lower(name)`)
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

func (s *Store) TargetByID(ctx context.Context, id int64) (*Target, error) {
	return scanTarget(s.db.QueryRow(ctx, `SELECT `+targetCols+` FROM targets WHERE id=$1`, id))
}

// Sealer chiffre un secret lié à l'identifiant de la cible (connu seulement après l'insertion).
type Sealer func(targetID int64) (secret, passphrase []byte, err error)

// SaveTarget crée (ID = 0) ou met à jour une cible. Si seal est nil, les secrets existants sont conservés.
func (s *Store) SaveTarget(ctx context.Context, t *Target, seal Sealer) (int64, error) {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if t.ID == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO targets (name, protocol, host, port, username, auth_method, host_key, description, keyboard_layout, folder)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
				t.Name, t.Protocol, t.Host, t.Port, t.Username, t.AuthMethod, t.HostKey, t.Description, t.Layout, t.Folder).Scan(&t.ID); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE targets SET name=$2, protocol=$3, host=$4, port=$5, username=$6,
				auth_method=$7, host_key=$8, description=$9, keyboard_layout=$10, folder=$11, updated_at=now() WHERE id=$1`,
				t.ID, t.Name, t.Protocol, t.Host, t.Port, t.Username, t.AuthMethod, t.HostKey, t.Description, t.Layout, t.Folder)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		if seal == nil {
			return nil
		}
		sec, pass, err := seal(t.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE targets SET secret=$2, passphrase=$3 WHERE id=$1`, t.ID, sec, pass)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, ErrDuplicate
	}
	return t.ID, err
}

func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM targets WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---------- Sessions d'accès ----------

func (s *Store) StartAccessSession(ctx context.Context, userID int64, username string, t *Target, ip string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO access_sessions (user_id, username, target_id, target_name, protocol, client_ip)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, userID, username, t.ID, t.Name, t.Protocol, ip).Scan(&id)
	return id, err
}

func (s *Store) EndAccessSession(ctx context.Context, id int64, reason string, in, out int64) error {
	_, err := s.db.Exec(ctx, `UPDATE access_sessions SET ended_at=now(), end_reason=$2, bytes_in=$3, bytes_out=$4
		WHERE id=$1 AND ended_at IS NULL`, id, truncate(reason, 200), in, out)
	return err
}

// CloseDanglingAccessSessions clôt les sessions restées ouvertes après un arrêt brutal.
func (s *Store) CloseDanglingAccessSessions(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE access_sessions SET ended_at=now(), end_reason='arrêt du service'
		WHERE ended_at IS NULL`)
	return tag.RowsAffected(), err
}

type AccessSession struct {
	ID              int64      `json:"id"`
	RecordingPath   string     `json:"-"`
	HasRecording    bool       `json:"has_recording"`
	RecordingSize   int64      `json:"recording_size"`
	RecordingPurged bool       `json:"recording_purged"`
	Username        string     `json:"username"`
	TargetName      string     `json:"target_name"`
	Protocol        string     `json:"protocol"`
	ClientIP        string     `json:"client_ip"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	EndReason       string     `json:"end_reason"`
	BytesIn         int64      `json:"bytes_in"`
	BytesOut        int64      `json:"bytes_out"`
}

func (s *Store) ListAccessSessions(ctx context.Context, limit int) ([]AccessSession, error) {
	rows, err := s.db.Query(ctx, `SELECT `+accessCols+` FROM access_sessions ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessSession{}
	for rows.Next() {
		a, err := scanAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

const accessCols = `id, username, target_name, protocol, client_ip, started_at, ended_at, end_reason,
	bytes_in, bytes_out, recording_path, recording_size, recording_purged`

func scanAccess(row pgx.Row) (*AccessSession, error) {
	a := &AccessSession{}
	err := row.Scan(&a.ID, &a.Username, &a.TargetName, &a.Protocol, &a.ClientIP, &a.StartedAt, &a.EndedAt,
		&a.EndReason, &a.BytesIn, &a.BytesOut, &a.RecordingPath, &a.RecordingSize, &a.RecordingPurged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	a.HasRecording = a.RecordingPath != ""
	return a, err
}

func (s *Store) AccessSessionByID(ctx context.Context, id int64) (*AccessSession, error) {
	return scanAccess(s.db.QueryRow(ctx, `SELECT `+accessCols+` FROM access_sessions WHERE id=$1`, id))
}

// RecordedSessionsSince renvoie les sessions enregistrées depuis une date (plus récentes d'abord).
func (s *Store) RecordedSessionsSince(ctx context.Context, since time.Time, limit int) ([]*AccessSession, error) {
	rows, err := s.db.Query(ctx, `SELECT `+accessCols+` FROM access_sessions
		WHERE recording_path <> '' AND protocol = 'ssh' AND started_at >= $1 ORDER BY id DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AccessSession
	for rows.Next() {
		a, err := scanAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SetRecording(ctx context.Context, id int64, path string, size int64) error {
	_, err := s.db.Exec(ctx, `UPDATE access_sessions SET recording_path=$2, recording_size=$3 WHERE id=$1`, id, path, size)
	return err
}

// ExpiredRecordings liste les enregistrements plus anciens que la durée de conservation.
func (s *Store) ExpiredRecordings(ctx context.Context, before time.Time) (map[int64]string, error) {
	rows, err := s.db.Query(ctx, `SELECT id, recording_path FROM access_sessions
		WHERE recording_path <> '' AND started_at < $1 AND ended_at IS NOT NULL`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

func (s *Store) MarkRecordingPurged(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `UPDATE access_sessions SET recording_path='', recording_size=0, recording_purged=TRUE WHERE id=$1`, id)
	return err
}
