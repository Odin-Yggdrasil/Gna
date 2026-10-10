package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gna/internal/recording"
	"gna/internal/store"
)

// loadRecording déchiffre l'enregistrement d'une session d'accès.
func (s *Server) loadRecording(w http.ResponseWriter, r *http.Request) (*store.AccessSession, []byte, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.AccessSessionByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session introuvable")
		return nil, nil, false
	}
	if err != nil {
		s.internalError(w, err)
		return nil, nil, false
	}
	if a.RecordingPath == "" {
		msg := "cette session n'a pas été enregistrée"
		if a.RecordingPurged {
			msg = "enregistrement supprimé (durée de conservation dépassée)"
		}
		writeError(w, http.StatusNotFound, msg)
		return nil, nil, false
	}
	cast, err := recording.ReadAll(a.RecordingPath, a.ID, s.auth.Box())
	if err != nil && !errors.Is(err, recording.ErrIncomplete) {
		s.internalError(w, err)
		return nil, nil, false
	}
	if errors.Is(err, recording.ErrIncomplete) {
		w.Header().Set("X-Gna-Recording", "incomplete")
	}
	return a, cast, true
}

func (s *Server) handleRecordingCast(w http.ResponseWriter, r *http.Request) {
	a, cast, ok := s.loadRecording(w, r)
	if !ok {
		return
	}
	if a.Protocol != "ssh" {
		writeError(w, http.StatusBadRequest, "ce n'est pas une session SSH")
		return
	}
	s.auditReq(r, "recording.viewed", map[string]any{"session": a.ID, "target": a.TargetName, "user": a.Username})
	w.Header().Set("Content-Type", "application/x-asciicast; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(cast)
}

func (s *Server) handleRecordingSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 {
		writeError(w, http.StatusBadRequest, "saisir au moins 2 caractères")
		return
	}
	a, cast, ok := s.loadRecording(w, r)
	if !ok {
		return
	}
	if a.Protocol != "ssh" {
		writeError(w, http.StatusBadRequest, "la recherche de texte ne concerne que les sessions SSH")
		return
	}
	s.auditReq(r, "recording.searched", map[string]any{"session": a.ID, "query": q})
	writeJSON(w, http.StatusOK, recording.Search(cast, q))
}

// handleSearchAllRecordings cherche un texte dans les enregistrements récents.
func (s *Server) handleSearchAllRecordings(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 {
		writeError(w, http.StatusBadRequest, "saisir au moins 2 caractères")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 3650 {
		days = 30
	}
	sessions, err := s.st.RecordedSessionsSince(r.Context(), time.Now().AddDate(0, 0, -days), 300)
	if err != nil {
		s.internalError(w, err)
		return
	}
	type result struct {
		store.AccessSession
		Total int             `json:"total"`
		Hits  []recording.Hit `json:"hits"`
	}
	out := []result{}
	for _, a := range sessions {
		if r.Context().Err() != nil {
			return
		}
		cast, err := recording.ReadAll(a.RecordingPath, a.ID, s.auth.Box())
		if err != nil && !errors.Is(err, recording.ErrIncomplete) {
			continue
		}
		hits := recording.Search(cast, q)
		if len(hits) == 0 {
			continue
		}
		res := result{AccessSession: *a, Total: len(hits), Hits: hits}
		if len(res.Hits) > 5 {
			res.Hits = res.Hits[:5]
		}
		out = append(out, res)
	}
	s.auditReq(r, "recording.searched", map[string]any{"query": q, "days": days, "sessions": len(out)})
	writeJSON(w, http.StatusOK, out)
}

// startRecording crée l'enregistrement d'une session d'accès, l'écrit sur disque toutes
// les 5 secondes (un arrêt brutal ne perd que les dernières secondes) et renvoie la
// fonction de clôture à appeler en fin de session.
func (s *Server) startRecording(ctx context.Context, accessID int64,
	create func(path string) (*recording.Writer, error)) (*recording.Writer, func(), error) {
	if accessID == 0 {
		return nil, nil, errors.New("session d'accès non journalisée")
	}
	path := recording.Path(s.cfg.RecordingsDir(), accessID, time.Now())
	w, err := create(path)
	if err != nil {
		return nil, nil, err
	}
	_ = s.st.SetRecording(ctx, accessID, path, 0)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := w.Flush(); err != nil {
					slog.Error("écriture de l'enregistrement", "session", accessID, "err", err)
				}
			}
		}
	}()
	finish := func() {
		size, err := w.Close()
		if err != nil {
			slog.Error("fermeture de l'enregistrement", "session", accessID, "err", err)
		}
		endCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.st.SetRecording(endCtx, accessID, path, size)
	}
	return w, finish, nil
}

// handleRecordingVNC renvoie l'enregistrement VNC déchiffré (flux binaire), que le
// navigateur réinjecte dans noVNC pour la relecture.
func (s *Server) handleRecordingVNC(w http.ResponseWriter, r *http.Request) {
	a, data, ok := s.loadRecording(w, r)
	if !ok {
		return
	}
	if a.Protocol != "vnc" && a.Protocol != "rdp" {
		writeError(w, http.StatusBadRequest, "ce n'est pas une session de bureau à distance")
		return
	}
	s.auditReq(r, "recording.viewed", map[string]any{"session": a.ID, "target": a.TargetName, "user": a.Username})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
