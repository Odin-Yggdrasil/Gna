package api

// Données personnelles : favoris, bibliothèque de commandes, espaces de travail.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gna/internal/store"
)

func (s *Server) handleSetFavorite(w http.ResponseWriter, r *http.Request) {
	t, ok := s.loadAllowedTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Favorite bool `json:"favorite"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.st.SetFavorite(r.Context(), identity(r).User.ID, t.ID, req.Favorite); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorite": req.Favorite})
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func personalError(s *Server, w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, what+" de ce nom existe déjà")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "introuvable")
	default:
		s.internalError(w, err)
	}
}

// ---------- bibliothèque de commandes ----------

func (s *Server) handleListSnippets(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Snippets(r.Context(), identity(r).User.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSaveSnippet(w http.ResponseWriter, r *http.Request) {
	var sn store.Snippet
	if !decode(w, r, &sn) {
		return
	}
	sn.ID = pathID(r) // 0 en création (POST)
	sn.Name = strings.TrimSpace(sn.Name)
	sn.Command = strings.TrimRight(strings.ReplaceAll(sn.Command, "\r\n", "\n"), "\n")
	switch {
	case len([]rune(sn.Name)) < 1 || len([]rune(sn.Name)) > 64:
		writeError(w, http.StatusBadRequest, "le nom doit faire entre 1 et 64 caractères")
		return
	case strings.TrimSpace(sn.Command) == "" || len(sn.Command) > 8000:
		writeError(w, http.StatusBadRequest, "la commande doit faire entre 1 et 8000 caractères")
		return
	}
	if err := s.st.SaveSnippet(r.Context(), identity(r).User.ID, &sn); err != nil {
		personalError(s, w, err, "une commande")
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (s *Server) handleDeleteSnippet(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteSnippet(r.Context(), identity(r).User.ID, pathID(r)); err != nil {
		personalError(s, w, err, "une commande")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- espaces de travail ----------

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Workspaces(r.Context(), identity(r).User.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleSaveWorkspace enregistre les sessions ouvertes sous un nom (remplace l'espace
// du même nom). Seules les machines auxquelles l'utilisateur a accès sont gardées.
func (s *Server) handleSaveWorkspace(w http.ResponseWriter, r *http.Request) {
	var ws store.Workspace
	if !decode(w, r, &ws) {
		return
	}
	ws.Name = strings.TrimSpace(ws.Name)
	if len([]rune(ws.Name)) < 1 || len([]rune(ws.Name)) > 64 {
		writeError(w, http.StatusBadRequest, "le nom doit faire entre 1 et 64 caractères")
		return
	}
	if ws.Layout != "grid" {
		ws.Layout = "tabs"
	}
	if len(ws.Items) == 0 || len(ws.Items) > 16 {
		writeError(w, http.StatusBadRequest, "un espace de travail contient de 1 à 16 sessions")
		return
	}
	ws.ID = pathID(r) // 0 = création (POST), sinon modification (PUT)
	user := identity(r).User
	for _, it := range ws.Items {
		if user.IsAdmin {
			if _, err := s.st.TargetByID(r.Context(), it.TargetID); err != nil {
				writeError(w, http.StatusBadRequest, "machine inconnue dans l'espace de travail")
				return
			}
		} else if ok, err := s.st.CanAccessTarget(r.Context(), user.ID, it.TargetID); err != nil || !ok {
			writeError(w, http.StatusBadRequest, "machine inconnue dans l'espace de travail")
			return
		}
	}
	var err error
	if ws.ID == 0 {
		err = s.st.SaveWorkspace(r.Context(), user.ID, &ws)
	} else {
		err = s.st.UpdateWorkspace(r.Context(), user.ID, &ws)
	}
	if err != nil {
		personalError(s, w, err, "un espace de travail")
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteWorkspace(r.Context(), identity(r).User.ID, pathID(r)); err != nil {
		personalError(s, w, err, "un espace de travail")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- préférences d'affichage ----------

// Thèmes proposés par l'interface (voir style.css).
var themes = map[string]bool{"system": true, "dark": true, "light": true, "nord": true, "dracula": true, "gruvbox": true, "solarized": true}

func (s *Server) handleSetPrefs(w http.ResponseWriter, r *http.Request) {
	var p store.Prefs
	if !decode(w, r, &p) {
		return
	}
	if !themes[p.Theme] {
		writeError(w, http.StatusBadRequest, "thème inconnu")
		return
	}
	if err := s.st.SetUserPrefs(r.Context(), identity(r).User.ID, p); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
