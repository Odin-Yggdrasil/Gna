package store

// Données personnelles d'un utilisateur : favoris, bibliothèque de commandes,
// espaces de travail. Chaque requête est limitée à l'utilisateur concerné.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------- favoris ----------

func (s *Store) Favorites(ctx context.Context, userID int64) (map[int64]bool, error) {
	rows, err := s.db.Query(ctx, `SELECT target_id FROM target_favorites WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) SetFavorite(ctx context.Context, userID, targetID int64, on bool) error {
	var err error
	if on {
		_, err = s.db.Exec(ctx, `INSERT INTO target_favorites (user_id, target_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, targetID)
	} else {
		_, err = s.db.Exec(ctx, `DELETE FROM target_favorites WHERE user_id=$1 AND target_id=$2`, userID, targetID)
	}
	return err
}

// ---------- bibliothèque de commandes ----------

type Snippet struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command"`
	Run     bool   `json:"run"`
}

func (s *Store) Snippets(ctx context.Context, userID int64) ([]Snippet, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, command, run FROM snippets WHERE user_id=$1 ORDER BY lower(name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snippet{}
	for rows.Next() {
		var sn Snippet
		if err := rows.Scan(&sn.ID, &sn.Name, &sn.Command, &sn.Run); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// SaveSnippet crée (ID = 0) ou modifie une commande de l'utilisateur.
func (s *Store) SaveSnippet(ctx context.Context, userID int64, sn *Snippet) error {
	var err error
	if sn.ID == 0 {
		err = s.db.QueryRow(ctx, `INSERT INTO snippets (user_id, name, command, run) VALUES ($1,$2,$3,$4) RETURNING id`,
			userID, sn.Name, sn.Command, sn.Run).Scan(&sn.ID)
	} else {
		var tag pgconn.CommandTag
		tag, err = s.db.Exec(ctx, `UPDATE snippets SET name=$3, command=$4, run=$5 WHERE id=$1 AND user_id=$2`,
			sn.ID, userID, sn.Name, sn.Command, sn.Run)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	if isUnique(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) DeleteSnippet(ctx context.Context, userID, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM snippets WHERE id=$1 AND user_id=$2`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---------- espaces de travail ----------

type WorkspaceItem struct {
	TargetID int64 `json:"target_id"`
	Sync     bool  `json:"sync"`
}

type Workspace struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Layout    string          `json:"layout"`
	Sync      bool            `json:"sync"`
	Items     []WorkspaceItem `json:"items"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (s *Store) Workspaces(ctx context.Context, userID int64) ([]Workspace, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, layout, sync, items, updated_at FROM workspaces WHERE user_id=$1 ORDER BY lower(name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		var items []byte
		if err := rows.Scan(&w.ID, &w.Name, &w.Layout, &w.Sync, &items, &w.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(items, &w.Items); err != nil || w.Items == nil {
			w.Items = []WorkspaceItem{}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SaveWorkspace enregistre un espace ; un espace du même nom est remplacé.
func (s *Store) SaveWorkspace(ctx context.Context, userID int64, w *Workspace) error {
	items, err := json.Marshal(w.Items)
	if err != nil {
		return err
	}
	return s.db.QueryRow(ctx, `INSERT INTO workspaces (user_id, name, layout, sync, items) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (user_id, name) DO UPDATE SET layout=EXCLUDED.layout, sync=EXCLUDED.sync, items=EXCLUDED.items, updated_at=now()
		RETURNING id, updated_at`, userID, w.Name, w.Layout, w.Sync, items).Scan(&w.ID, &w.UpdatedAt)
}

// UpdateWorkspace modifie un espace existant (renommage compris).
func (s *Store) UpdateWorkspace(ctx context.Context, userID int64, w *Workspace) error {
	items, err := json.Marshal(w.Items)
	if err != nil {
		return err
	}
	err = s.db.QueryRow(ctx, `UPDATE workspaces SET name=$3, layout=$4, sync=$5, items=$6, updated_at=now()
		WHERE id=$1 AND user_id=$2 RETURNING updated_at`, w.ID, userID, w.Name, w.Layout, w.Sync, items).Scan(&w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if isUnique(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) DeleteWorkspace(ctx context.Context, userID, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM workspaces WHERE id=$1 AND user_id=$2`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---------- préférences d'affichage ----------

type Prefs struct {
	Theme string `json:"theme"`
}

func (s *Store) UserPrefs(ctx context.Context, userID int64) (Prefs, error) {
	var raw []byte
	var p Prefs
	if err := s.db.QueryRow(ctx, `SELECT prefs FROM users WHERE id=$1`, userID).Scan(&raw); err != nil {
		return p, err
	}
	_ = json.Unmarshal(raw, &p)
	return p, nil
}

func (s *Store) SetUserPrefs(ctx context.Context, userID int64, p Prefs) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE users SET prefs=$2 WHERE id=$1`, userID, raw)
	return err
}
