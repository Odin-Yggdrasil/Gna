package api

// Jetons d'accès personnels : un script (menu du bureau…) lit la liste des machines de
// l'utilisateur avec l'en-tête « Authorization: Bearer gna_… », sans mot de passe ni TOTP.
//
// Portée volontairement minimale : seules GET /api/me et GET /api/targets acceptent un
// jeton (voir readable). Toutes les autres routes passent par require, qui ne lit que le
// cookie de session : un jeton n'y ouvre ni terminal, ni bureau, ni SFTP, ni administration.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"gna/internal/auth"
	"gna/internal/store"
)

// readable accepte, en plus d'une session à l'étape indiquée, un jeton d'accès personnel.
// Chaque utilisation d'un jeton est limitée en débit par IP et inscrite au journal.
func (s *Server) readable(stage string, h http.HandlerFunc) http.Handler {
	withSession := s.require(stage, h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if authz == "" {
			withSession.ServeHTTP(w, r)
			return
		}
		ip := s.clientIP(r)
		if !s.tokenLimiter.allow(ip) {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusTooManyRequests, "trop de requêtes, réessayez dans un instant")
			return
		}
		var ident *auth.Identity
		err := auth.ErrUnauthenticated
		if token, ok := strings.CutPrefix(authz, "Bearer "); ok {
			ident, err = s.auth.AuthenticateAPIToken(r.Context(), strings.TrimSpace(token), s.client(r))
		}
		if errors.Is(err, auth.ErrUnauthenticated) {
			_ = s.st.Audit(r.Context(), nil, "", "api_token.denied", ip, map[string]any{"path": r.URL.Path})
			w.Header().Set("WWW-Authenticate", `Bearer realm="gna"`)
			writeError(w, http.StatusUnauthorized, "jeton d'accès invalide ou révoqué")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		_ = s.st.Audit(r.Context(), &ident.User.ID, ident.User.Username, "api_token.used", ip,
			map[string]any{"token": ident.APIToken.Name, "path": r.URL.Path})
		h(w, r.WithContext(context.WithValue(r.Context(), identityKey, ident)))
	})
}

// ---------- gestion des jetons (Paramètres › Jetons d'accès, session uniquement) ----------

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.APITokens(r.Context(), identity(r).User.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateToken renvoie le jeton en clair une seule fois ; seule son empreinte est gardée.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		writeError(w, http.StatusBadRequest, "nom obligatoire (64 caractères au plus)")
		return
	}
	token, hash, err := auth.NewAPIToken()
	if err != nil {
		s.internalError(w, err)
		return
	}
	user := identity(r).User
	t, err := s.st.CreateAPIToken(r.Context(), user.ID, name, hash)
	if errors.Is(err, store.ErrLimit) {
		writeError(w, http.StatusConflict, "nombre maximal de jetons atteint ("+strconv.Itoa(store.MaxAPITokens)+") : révoquez-en un")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "api_token.created", map[string]any{"token": name, "token_id": t.ID})
	writeJSON(w, http.StatusOK, struct {
		*store.APIToken
		Token string `json:"token"`
	}{t, token})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name, err := s.st.DeleteAPIToken(r.Context(), identity(r).User.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "jeton introuvable")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "api_token.revoked", map[string]any{"token": name, "token_id": id})
	w.WriteHeader(http.StatusNoContent)
}
