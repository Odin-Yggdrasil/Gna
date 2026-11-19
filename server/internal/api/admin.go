package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gna/internal/auth"
	"gna/internal/secure"
	"gna/internal/store"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9._-]{2,64}$`)

// ---------- Utilisateurs ----------

type userJSON struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"is_admin"`
	Disabled  bool      `json:"disabled"`
	MFA       bool      `json:"mfa"`
	Locked    bool      `json:"locked"`
	Groups    []int64   `json:"groups"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	groups, err := s.st.UserGroups(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]userJSON, len(users))
	for i, u := range users {
		g := groups[u.ID]
		if g == nil {
			g = []int64{}
		}
		out[i] = userJSON{ID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin, Disabled: u.Disabled,
			MFA: u.TOTPEnabled, Locked: u.LockedUntil != nil && u.LockedUntil.After(time.Now()),
			Groups: g, CreatedAt: u.CreatedAt}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if !decode(w, r, &in) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Username))
	if !usernameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "identifiant invalide : 2 à 64 caractères parmi a-z, 0-9, « . », « _ », « - »")
		return
	}
	if err := secure.CheckPasswordPolicy(in.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := secure.HashPassword(in.Password)
	if err != nil {
		s.internalError(w, err)
		return
	}
	id, err := s.st.CreateUser(r.Context(), name, hash, in.IsAdmin)
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, "cet identifiant existe déjà")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "user.created", map[string]any{"user": name, "admin": in.IsAdmin})
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleUserAction regroupe les actions d'administration sur un compte.
func (s *Server) handleUserAction(w http.ResponseWriter, r *http.Request) {
	me := identity(r).User
	uid, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := s.st.UserByID(r.Context(), uid)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "utilisateur introuvable")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	action := r.PathValue("action")
	self := u.ID == me.ID

	// Garde-fous : ne pas se retirer ses propres droits, garder au moins un admin actif.
	removesAdmin := u.IsAdmin && !u.Disabled && (action == "demote" || action == "disable" || action == "delete")
	if self && (action == "demote" || action == "disable" || action == "delete") {
		writeError(w, http.StatusBadRequest, "impossible sur votre propre compte")
		return
	}
	if removesAdmin {
		n, err := s.st.CountOtherActiveAdmins(r.Context(), u.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if n == 0 {
			writeError(w, http.StatusBadRequest, "il doit rester au moins un administrateur actif")
			return
		}
	}

	ctx := r.Context()
	details := map[string]any{"user": u.Username}
	switch action {
	case "promote", "demote":
		err = s.st.SetAdmin(ctx, u.ID, action == "promote")
	case "disable", "enable":
		err = s.st.SetDisabled(ctx, u.ID, action == "disable")
	case "unlock":
		err = s.st.ResetLoginFailures(ctx, u.ID)
	case "reset-mfa":
		err = s.st.ResetTOTP(ctx, u.ID)
	case "password":
		var in struct {
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := secure.CheckPasswordPolicy(in.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		hash, herr := secure.HashPassword(in.Password)
		if herr != nil {
			s.internalError(w, herr)
			return
		}
		err = s.st.SetPassword(ctx, u.ID, hash) // ferme aussi ses sessions
	case "delete":
		err = s.st.DeleteUser(ctx, u.ID)
	default:
		writeError(w, http.StatusNotFound, "action inconnue")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "user."+strings.ReplaceAll(action, "-", "_"), details)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Mon compte ----------

func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	ok, err := secure.VerifyPassword(in.Current, id.User.PasswordHash)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !ok {
		s.auditReq(r, "account.password_change_failed", nil)
		writeError(w, http.StatusBadRequest, "mot de passe actuel incorrect")
		return
	}
	if err := secure.CheckPasswordPolicy(in.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.New == in.Current {
		writeError(w, http.StatusBadRequest, "le nouveau mot de passe doit être différent")
		return
	}
	hash, err := secure.HashPassword(in.New)
	if err != nil {
		s.internalError(w, err)
		return
	}
	// Les autres sessions (autres appareils) sont fermées, celle-ci est conservée.
	if err := s.st.SetPasswordKeepSession(r.Context(), id.User.ID, hash, auth.HashToken(id.Token)); err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "account.password_changed", nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Groupes ----------

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.st.ListGroups(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) handleSaveGroup(w http.ResponseWriter, r *http.Request) {
	var in store.Group
	if !decode(w, r, &in) {
		return
	}
	in.ID = 0
	if r.PathValue("id") != "" {
		in.ID, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if n := len([]rune(in.Name)); n < 1 || n > 64 {
		writeError(w, http.StatusBadRequest, "le nom doit faire entre 1 et 64 caractères")
		return
	}
	if len(in.Description) > 500 {
		writeError(w, http.StatusBadRequest, "description trop longue")
		return
	}
	creating := in.ID == 0
	id, err := s.st.SaveGroup(r.Context(), &in)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, "un groupe porte déjà ce nom")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "groupe introuvable")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	event := "group.updated"
	if creating {
		event = "group.created"
	}
	s.auditReq(r, event, map[string]any{"group": in.Name, "members": in.Members, "targets": in.Targets})
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name, err := s.st.DeleteGroup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "groupe introuvable")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "group.deleted", map[string]any{"group": name})
	w.WriteHeader(http.StatusNoContent)
}
