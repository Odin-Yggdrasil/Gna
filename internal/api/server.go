// Package api expose l'API HTTP de Gná et sert l'interface web.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"gna/internal/auth"
	"gna/internal/config"
	"gna/internal/store"
	"gna/internal/transfer"
)

type ctxKey int

const identityKey ctxKey = 0

type Server struct {
	cfg     *config.Config
	st      *store.Store
	auth    *auth.Service
	static  fs.FS
	limiter *rateLimiter
	xfer    *transfer.Manager // nil : transfert de fichiers désactivé
	sftp    sftpRegistry      // transferts SFTP des sessions SSH ouvertes
}

func New(cfg *config.Config, st *store.Store, a *auth.Service, static fs.FS) *Server {
	s := &Server{cfg: cfg, st: st, auth: a, static: static, limiter: newRateLimiter(20, 10)}
	if cfg.FileTransfer {
		x, err := transfer.NewManager(cfg.TransfersDir(), int64(cfg.TransferMaxMB)<<20)
		if err != nil {
			slog.Error("transfert de fichiers désactivé", "err", err)
		} else {
			s.xfer = x
		}
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Authentification (limitée en débit par IP)
	mux.Handle("POST /api/auth/login", s.limited(http.HandlerFunc(s.handleLogin)))
	mux.Handle("POST /api/auth/totp/verify", s.limited(s.require(auth.StageMFA, s.handleTOTPVerify)))
	mux.Handle("POST /api/auth/totp/enroll", s.limited(s.require(auth.StageEnroll, s.handleEnrollStart)))
	mux.Handle("POST /api/auth/totp/enroll/confirm", s.limited(s.require(auth.StageEnroll, s.handleEnrollConfirm)))
	mux.Handle("POST /api/auth/logout", s.require("", s.handleLogout))
	mux.Handle("POST /api/auth/unlock", s.limited(s.require(auth.StageLocked, s.handleUnlock)))
	mux.Handle("POST /api/auth/ping", http.HandlerFunc(s.handlePing))

	// Session en cours (toute étape : l'interface s'en sert pour savoir quoi afficher)
	mux.Handle("GET /api/me", s.require("", s.handleMe))
	mux.Handle("PUT /api/me/prefs", s.require(auth.StageFull, s.handleSetPrefs))
	mux.Handle("POST /api/me/password", s.limited(s.require(auth.StageFull, s.handleChangeOwnPassword)))

	// Cibles et terminal
	mux.Handle("GET /api/targets", s.require(auth.StageFull, s.handleListTargets))
	mux.Handle("GET /api/targets/{id}/ssh", s.require(auth.StageFull, s.handleSSHTerminal))
	mux.Handle("GET /api/targets/{id}/vnc", s.require(auth.StageFull, s.handleVNC))
	mux.Handle("PUT /api/targets/{id}/favorite", s.require(auth.StageFull, s.handleSetFavorite))

	// Données personnelles : commandes et espaces de travail
	mux.Handle("GET /api/snippets", s.require(auth.StageFull, s.handleListSnippets))
	mux.Handle("POST /api/snippets", s.require(auth.StageFull, s.handleSaveSnippet))
	mux.Handle("PUT /api/snippets/{id}", s.require(auth.StageFull, s.handleSaveSnippet))
	mux.Handle("DELETE /api/snippets/{id}", s.require(auth.StageFull, s.handleDeleteSnippet))
	mux.Handle("GET /api/workspaces", s.require(auth.StageFull, s.handleListWorkspaces))
	mux.Handle("POST /api/workspaces", s.require(auth.StageFull, s.handleSaveWorkspace))
	mux.Handle("PUT /api/workspaces/{id}", s.require(auth.StageFull, s.handleSaveWorkspace))
	mux.Handle("DELETE /api/workspaces/{id}", s.require(auth.StageFull, s.handleDeleteWorkspace))
	mux.Handle("GET /api/transfers/{tid}", s.require(auth.StageFull, s.handleTransferList))
	mux.Handle("POST /api/transfers/{tid}/files", s.require(auth.StageFull, s.handleTransferUpload))
	mux.Handle("GET /api/transfers/{tid}/files", s.require(auth.StageFull, s.handleTransferDownload))
	mux.Handle("GET /api/sftp/{tid}/list", s.require(auth.StageFull, s.handleSFTPList))
	mux.Handle("GET /api/sftp/{tid}/file", s.require(auth.StageFull, s.handleSFTPDownload))
	mux.Handle("POST /api/sftp/{tid}/file", s.require(auth.StageFull, s.handleSFTPUpload))
	mux.Handle("POST /api/sftp/{tid}/mkdir", s.require(auth.StageFull, s.handleSFTPMkdir))

	// Administration
	mux.Handle("GET /api/admin/audit", s.require(auth.StageFull, s.admin(s.handleAudit)))
	mux.Handle("GET /api/admin/access-sessions", s.require(auth.StageFull, s.admin(s.handleAccessSessions)))
	mux.Handle("POST /api/admin/targets", s.require(auth.StageFull, s.admin(s.handleSaveTarget)))
	mux.Handle("PUT /api/admin/targets/{id}", s.require(auth.StageFull, s.admin(s.handleSaveTarget)))
	mux.Handle("DELETE /api/admin/targets/{id}", s.require(auth.StageFull, s.admin(s.handleDeleteTarget)))
	mux.Handle("GET /api/admin/recordings/search", s.require(auth.StageFull, s.admin(s.handleSearchAllRecordings)))
	mux.Handle("GET /api/admin/recordings/{id}/cast", s.require(auth.StageFull, s.admin(s.handleRecordingCast)))
	mux.Handle("GET /api/admin/recordings/{id}/vnc", s.require(auth.StageFull, s.admin(s.handleRecordingVNC)))
	mux.Handle("GET /api/admin/recordings/{id}/search", s.require(auth.StageFull, s.admin(s.handleRecordingSearch)))
	mux.Handle("GET /api/admin/users", s.require(auth.StageFull, s.admin(s.handleListUsers)))
	mux.Handle("POST /api/admin/users", s.require(auth.StageFull, s.admin(s.handleCreateUser)))
	mux.Handle("POST /api/admin/users/{id}/{action}", s.require(auth.StageFull, s.admin(s.handleUserAction)))
	mux.Handle("GET /api/admin/groups", s.require(auth.StageFull, s.admin(s.handleListGroups)))
	mux.Handle("POST /api/admin/groups", s.require(auth.StageFull, s.admin(s.handleSaveGroup)))
	mux.Handle("PUT /api/admin/groups/{id}", s.require(auth.StageFull, s.admin(s.handleSaveGroup)))
	mux.Handle("DELETE /api/admin/groups/{id}", s.require(auth.StageFull, s.admin(s.handleDeleteGroup)))
	mux.Handle("POST /api/admin/targets/scan", s.limited(s.require(auth.StageFull, s.admin(s.handleScanHostKey))))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /", http.FileServerFS(s.static))

	return s.securityHeaders(s.csrf(mux))
}

// ---------- Middlewares ----------

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; connect-src 'self'; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		} else {
			// Interface : toujours revérifiée, pour qu'une mise à jour de Gná soit prise
			// en compte sans vider le cache du navigateur.
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// csrf : en plus du cookie SameSite=Strict, toute requête qui modifie l'état doit
// être en JSON et, si le navigateur envoie un en-tête Origin, venir du même hôte.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					writeError(w, http.StatusForbidden, "origine refusée")
					return
				}
			}
			// Exception : l'envoi de fichier (corps brut). Il exige un en-tête propre à
			// Gná, qu'un autre site ne peut pas ajouter sans autorisation CORS.
			upload := (strings.HasPrefix(r.URL.Path, "/api/transfers/") || strings.HasPrefix(r.URL.Path, "/api/sftp/")) &&
				r.Header.Get("X-Gna-Upload") == "1"
			if r.Method != http.MethodDelete && !upload && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "le corps doit être en JSON")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(s.clientIP(r)) {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusTooManyRequests, "trop de tentatives, réessayez dans un instant")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// require exige une session valide, à l'étape indiquée ("" = n'importe laquelle).
func (s *Server) require(stage string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.auth.Authenticate(r.Context(), s.readCookie(r))
		if errors.Is(err, auth.ErrUnauthenticated) {
			s.clearCookie(w)
			writeError(w, http.StatusUnauthorized, "non authentifié")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		if id.Session.Stage == auth.StageLocked && stage != "" && stage != auth.StageLocked {
			// Session verrouillée par inactivité : le cookie est gardé, l'interface
			// affiche l'écran de déverrouillage (mot de passe seul).
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session verrouillée après inactivité", "locked": true})
			return
		}
		if stage != "" && id.Session.Stage != stage {
			writeError(w, http.StatusForbidden, "étape d'authentification incorrecte")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !identity(r).User.IsAdmin {
			writeError(w, http.StatusForbidden, "réservé aux administrateurs")
			return
		}
		h(w, r)
	}
}

func identity(r *http.Request) *auth.Identity { return r.Context().Value(identityKey).(*auth.Identity) }

// ---------- Handlers ----------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if !decode(w, r, &req) {
		return
	}
	token, stage, err := s.auth.Login(r.Context(), strings.ToLower(strings.TrimSpace(req.Username)), req.Password, s.client(r))
	if errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "identifiant ou mot de passe incorrect")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.setCookie(w, token, 5*time.Minute)
	writeJSON(w, http.StatusOK, map[string]string{"stage": stage})
}

func (s *Server) handleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	if !decode(w, r, &req) {
		return
	}
	token, err := s.auth.VerifyTOTP(r.Context(), identity(r), strings.TrimSpace(req.Code), s.client(r))
	s.finishPromotion(w, token, err)
}

func (s *Server) handleEnrollStart(w http.ResponseWriter, r *http.Request) {
	uri, secret, err := s.auth.StartEnrollment(r.Context(), identity(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	png, err := qrcode.Encode(uri, qrcode.Medium, 256)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret,
		"uri":    uri,
		"qr":     "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

func (s *Server) handleEnrollConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	if !decode(w, r, &req) {
		return
	}
	token, err := s.auth.ConfirmEnrollment(r.Context(), identity(r), strings.TrimSpace(req.Code), s.client(r))
	s.finishPromotion(w, token, err)
}

func (s *Server) finishPromotion(w http.ResponseWriter, token string, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCode):
		writeError(w, http.StatusUnauthorized, "code invalide")
	case errors.Is(err, auth.ErrUnauthenticated):
		s.clearCookie(w)
		writeError(w, http.StatusUnauthorized, "session expirée, reconnectez-vous")
	case err != nil:
		s.internalError(w, err)
	default:
		s.setCookie(w, token, s.cfg.SessionTTL)
		writeJSON(w, http.StatusOK, map[string]string{"stage": auth.StageFull})
	}
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if !decode(w, r, &req) {
		return
	}
	token, expires, err := s.auth.Unlock(r.Context(), identity(r), req.Password, s.client(r))
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "mot de passe incorrect")
	case errors.Is(err, auth.ErrUnauthenticated):
		s.clearCookie(w)
		writeError(w, http.StatusUnauthorized, "session expirée, reconnectez-vous")
	case err != nil:
		s.internalError(w, err)
	default:
		s.setCookie(w, token, time.Until(expires))
		writeJSON(w, http.StatusOK, map[string]string{"stage": auth.StageFull})
	}
}

// handlePing : vérification périodique de l'interface. Elle ne compte comme activité que
// si l'utilisateur a réellement agi (clavier, souris, toucher) depuis la précédente.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	var req struct{ Active bool }
	if !decode(w, r, &req) {
		return
	}
	id, err := s.auth.Check(r.Context(), s.readCookie(r), req.Active)
	if errors.Is(err, auth.ErrUnauthenticated) {
		s.clearCookie(w)
		writeError(w, http.StatusUnauthorized, "non authentifié")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := map[string]any{"stage": id.Session.Stage}
	if id.Session.Stage == auth.StageFull {
		out["idle_left_s"] = int(s.auth.IdleLeft(id.Session).Seconds())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), identity(r), s.client(r)); err != nil {
		s.internalError(w, err)
		return
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	out := map[string]any{"stage": id.Session.Stage, "username": id.User.Username, "id": id.User.ID}
	if id.Session.Stage == auth.StageFull {
		out["is_admin"] = id.User.IsAdmin
		out["expires_at"] = id.Session.ExpiresAt
		out["idle_timeout_s"] = int(s.cfg.IdleTimeout.Seconds())
		out["recording"] = map[string]bool{"ssh": s.cfg.RecordSessions, "vnc": s.cfg.RecordVNC}
		if prefs, err := s.st.UserPrefs(r.Context(), id.User.ID); err == nil {
			out["prefs"] = prefs
		}
		out["tools"] = map[string]any{
			"clipboard":       s.cfg.Clipboard,
			"file_transfer":   s.xfer != nil,
			"sftp":            s.cfg.FileTransfer,
			"transfer_max_mb": s.cfg.TransferMaxMB,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, err := s.st.ListAudit(r.Context(), limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ---------- Cookies et utilitaires ----------

func (s *Server) cookieName() string {
	if s.cfg.CookieSecure {
		return "__Host-gna_session" // préfixe __Host- : Secure, Path=/, sans Domain
	}
	return "gna_session"
}

func (s *Server) readCookie(r *http.Request) string {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) setCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

// clientIP : l'adresse TCP, ou la dernière entrée de X-Forwarded-For si Gná est
// derrière un reverse proxy de confiance (celle ajoutée par le proxy lui-même).
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) client(r *http.Request) auth.Client {
	return auth.Client{IP: s.clientIP(r), UserAgent: r.UserAgent()}
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	slog.Error("erreur interne", "err", err)
	writeError(w, http.StatusInternalServerError, "erreur interne")
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "requête JSON invalide")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
