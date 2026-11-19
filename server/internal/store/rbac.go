package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------- Contrôle d'accès ----------

const accessCond = `EXISTS (
	SELECT 1 FROM target_access ta
	WHERE ta.target_id = t.id
	  AND (ta.user_id = $1 OR ta.group_id IN (SELECT group_id FROM group_members WHERE user_id = $1)))`

// ListTargetsForUser renvoie les cibles attribuées à un utilisateur (directement ou via un groupe).
func (s *Store) ListTargetsForUser(ctx context.Context, userID int64) ([]*Target, error) {
	rows, err := s.db.Query(ctx, `SELECT `+targetCols+` FROM targets t WHERE `+accessCond+` ORDER BY lower(name)`, userID)
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

// CanAccessTarget indique si un utilisateur non-admin a accès à une cible.
func (s *Store) CanAccessTarget(ctx context.Context, userID, targetID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM targets t WHERE t.id = $2 AND `+accessCond+`)`,
		userID, targetID).Scan(&ok)
	return ok, err
}

// Access liste les utilisateurs et groupes autorisés sur une cible.
type Access struct {
	Users  []int64 `json:"users"`
	Groups []int64 `json:"groups"`
}

// AllTargetAccess renvoie les attributions de toutes les cibles, indexées par cible.
func (s *Store) AllTargetAccess(ctx context.Context) (map[int64]*Access, error) {
	rows, err := s.db.Query(ctx, `SELECT target_id, user_id, group_id FROM target_access ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*Access{}
	for rows.Next() {
		var tid int64
		var uid, gid *int64
		if err := rows.Scan(&tid, &uid, &gid); err != nil {
			return nil, err
		}
		a := out[tid]
		if a == nil {
			a = &Access{Users: []int64{}, Groups: []int64{}}
			out[tid] = a
		}
		if uid != nil {
			a.Users = append(a.Users, *uid)
		} else if gid != nil {
			a.Groups = append(a.Groups, *gid)
		}
	}
	return out, rows.Err()
}

// SetTargetAccess remplace la liste des utilisateurs et groupes autorisés sur une cible.
func (s *Store) SetTargetAccess(ctx context.Context, targetID int64, a Access) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM target_access WHERE target_id=$1`, targetID); err != nil {
			return err
		}
		// Les identifiants inexistants sont ignorés silencieusement (jointure).
		if _, err := tx.Exec(ctx, `INSERT INTO target_access (target_id, user_id)
			SELECT $1, id FROM users WHERE id = ANY($2)`, targetID, nonNil(a.Users)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO target_access (target_id, group_id)
			SELECT $1, id FROM groups WHERE id = ANY($2)`, targetID, nonNil(a.Groups))
		return err
	})
}

// ---------- Groupes ----------

type Group struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Members     []int64   `json:"members"`
	Targets     []int64   `json:"targets"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Store) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id, g.name, g.description, g.created_at,
			COALESCE((SELECT array_agg(user_id ORDER BY user_id) FROM group_members WHERE group_id = g.id), '{}'),
			COALESCE((SELECT array_agg(target_id ORDER BY target_id) FROM target_access WHERE group_id = g.id), '{}')
		FROM groups g ORDER BY lower(g.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Group{}
	for rows.Next() {
		g := &Group{}
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.Members, &g.Targets); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SaveGroup crée (ID = 0) ou met à jour un groupe, avec ses membres et ses cibles.
func (s *Store) SaveGroup(ctx context.Context, g *Group) (int64, error) {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if g.ID == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO groups (name, description) VALUES ($1,$2) RETURNING id`,
				g.Name, g.Description).Scan(&g.ID); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE groups SET name=$2, description=$3 WHERE id=$1`, g.ID, g.Name, g.Description)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		stmts := []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM group_members WHERE group_id=$1`, []any{g.ID}},
			{`INSERT INTO group_members (group_id, user_id) SELECT $1, id FROM users WHERE id = ANY($2)`, []any{g.ID, nonNil(g.Members)}},
			{`DELETE FROM target_access WHERE group_id=$1`, []any{g.ID}},
			{`INSERT INTO target_access (target_id, group_id) SELECT id, $1 FROM targets WHERE id = ANY($2)`, []any{g.ID, nonNil(g.Targets)}},
		}
		for _, st := range stmts {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if isUnique(err) {
		return 0, ErrDuplicate
	}
	return g.ID, err
}

func (s *Store) DeleteGroup(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `DELETE FROM groups WHERE id=$1 RETURNING name`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// ---------- Utilisateurs (administration) ----------

// UserGroups renvoie, pour chaque utilisateur, la liste des groupes dont il est membre.
func (s *Store) UserGroups(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT user_id, group_id FROM group_members ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var u, g int64
		if err := rows.Scan(&u, &g); err != nil {
			return nil, err
		}
		out[u] = append(out[u], g)
	}
	return out, rows.Err()
}

func (s *Store) SetAdmin(ctx context.Context, id int64, admin bool) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET is_admin=$2, updated_at=now() WHERE id=$1`, id, admin)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// CountOtherActiveAdmins compte les administrateurs actifs autres que exceptID.
func (s *Store) CountOtherActiveAdmins(ctx context.Context, exceptID int64) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_admin AND NOT disabled AND id <> $1`, exceptID).Scan(&n)
	return n, err
}

// SetPasswordKeepSession change le mot de passe et ferme toutes les sessions sauf celle indiquée.
func (s *Store) SetPasswordKeepSession(ctx context.Context, id int64, hash string, keepTokenHash []byte) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1`, id, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1 AND token_hash <> $2`, id, keepTokenHash)
		return err
	})
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
