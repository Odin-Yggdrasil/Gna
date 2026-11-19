package api

// Liaisons Runa : un autre outil de la famille (Völund) ajoute ou met à jour des machines
// avec un jeton de service « Authorization: Bearer gnal_… ». Un administrateur crée la
// liaison (Paramètres › Liaisons) et colle le code d'appairage dans l'autre outil. Format
// du code : voir Runa/docs/ecosysteme.md (« runa1. » suivi d'un JSON en base64url).
//
// Portées :
//   - targets:write (Völund), une seule route (PUT /api/link/targets/{ref}) qui ne touche ni
//     aux identifiants de connexion, ni aux droits, ni à la suppression. Une machine ajoutée
//     ainsi n'a pas d'identifiants : elle reste « à compléter ».
//   - secrets:write (Andvari) : POST /api/link/secrets reçoit un mot de passe chiffré par
//     l'app Andvari pour la clé de Gná (boîte scellée Runa ; le serveur Andvari, qui
//     transmet, ne peut pas le lire). Gná en fait le mot de passe sudo des machines
//     correspondantes, et leurs identifiants si elles étaient « à compléter ».

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gna/internal/auth"
	"gna/internal/runabox"
	"gna/internal/sshproxy"
	"gna/internal/store"
	"gna/internal/tlsutil"
)

const (
	ScopeTargetsWrite = "targets:write"
	ScopeSecretsWrite = "secrets:write"
	linkTokenPrefix   = "gnal_"
	pairingTTL        = 15 * time.Minute
	secretBoxAAD      = "runa/gna/secret/v1"
)

var linkScopes = map[string]bool{ScopeTargetsWrite: true, ScopeSecretsWrite: true}

// boxKey : clé de réception de Gná (P-256), créée au premier usage dans le dossier de
// données. Sa clé publique figure dans les codes d'appairage portant secrets:write.
func (s *Server) boxKey() (*ecdh.PrivateKey, error) {
	s.boxOnce.Do(func() {
		path := filepath.Join(s.cfg.DataDir, "liaisons.key")
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			var k *ecdh.PrivateKey
			if k, err = ecdh.P256().GenerateKey(rand.Reader); err == nil {
				raw = k.Bytes()
				if err = os.MkdirAll(s.cfg.DataDir, 0o700); err == nil {
					err = os.WriteFile(path, raw, 0o600)
				}
			}
		}
		if err == nil {
			s.boxPriv, err = ecdh.P256().NewPrivateKey(raw)
		}
		s.boxErr = err
	})
	return s.boxPriv, s.boxErr
}

var linkRefRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (s *Server) linkRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/admin/links", s.require(auth.StageFull, s.admin(s.handleListLinks)))
	mux.Handle("POST /api/admin/links", s.limited(s.require(auth.StageFull, s.admin(s.handleCreateLink))))
	mux.Handle("DELETE /api/admin/links/{id}", s.require(auth.StageFull, s.admin(s.handleDeleteLink)))
	mux.Handle("GET /api/link", s.linked("", s.handleLinkInfo))
	mux.Handle("PUT /api/link/targets/{ref}", s.linked(ScopeTargetsWrite, s.handleLinkedTarget))
	mux.Handle("POST /api/link/secrets", s.linked(ScopeSecretsWrite, s.handleLinkedSecret))
}

type linkHandler func(http.ResponseWriter, *http.Request, *store.Link)

// linked authentifie un jeton de liaison portant la portée demandée (vide : n'importe laquelle).
func (s *Server) linked(scope string, h linkHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		if !s.tokenLimiter.allow(ip) {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusTooManyRequests, "trop de requêtes, réessayez dans un instant")
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		var l *store.Link
		err := store.ErrNotFound
		if strings.HasPrefix(token, linkTokenPrefix) {
			l, err = s.st.UseLink(r.Context(), auth.HashToken(token), ip)
		}
		if errors.Is(err, store.ErrNotFound) {
			_ = s.st.Audit(r.Context(), nil, "", "link.denied", ip, map[string]any{"path": r.URL.Path})
			w.Header().Set("WWW-Authenticate", `Bearer realm="gna"`)
			writeError(w, http.StatusUnauthorized, "jeton de liaison invalide ou révoqué")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		if scope != "" && !l.Has(scope) {
			writeError(w, http.StatusForbidden, "cette liaison n'a pas les droits nécessaires")
			return
		}
		h(w, r, l)
	})
}

func (s *Server) handleLinkInfo(w http.ResponseWriter, r *http.Request, l *store.Link) {
	writeJSON(w, http.StatusOK, map[string]any{"outil": "gna", "liaison": l.Name, "scopes": l.Scopes})
}

// handleLinkedTarget crée ou met à jour une machine SSH envoyée par l'outil lié.
func (s *Server) handleLinkedTarget(w http.ResponseWriter, r *http.Request, l *store.Link) {
	var in struct {
		Name        string `json:"name"`
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Username    string `json:"username"`
		HostKey     string `json:"host_key"`
		Description string `json:"description"`
		Folder      string `json:"folder"`
	}
	if !decode(w, r, &in) {
		return
	}
	ref := r.PathValue("ref")
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
	in.HostKey = strings.TrimSpace(in.HostKey)
	in.Description = strings.TrimSpace(in.Description)
	in.Folder = strings.Join(strings.Fields(in.Folder), " ")
	if in.Port == 0 {
		in.Port = 22
	}
	if in.Username == "" {
		in.Username = "root"
	}
	var msg string
	switch {
	case !linkRefRe.MatchString(ref):
		msg = "référence invalide"
	case utf8.RuneCountInString(in.Name) < 1 || utf8.RuneCountInString(in.Name) > 64:
		msg = "le nom doit faire entre 1 et 64 caractères"
	case !hostRe.MatchString(in.Host):
		msg = "adresse invalide (nom d'hôte ou IP)"
	case in.Port < 1 || in.Port > 65535:
		msg = "port invalide"
	case len(in.Username) > 64:
		msg = "identifiant trop long"
	case len(in.Description) > 500:
		msg = "description trop longue"
	case utf8.RuneCountInString(in.Folder) > 64:
		msg = "nom de dossier trop long (64 caractères au plus)"
	}
	if msg == "" {
		if _, err := sshproxy.ParseHostKey(in.HostKey); err != nil {
			msg = "clé d'hôte SSH manquante ou invalide"
		}
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, created, err := s.st.UpsertLinkedTarget(r.Context(), l.ID, store.LinkedTarget{Ref: ref, Name: in.Name, Host: in.Host,
		Port: in.Port, Username: in.Username, HostKey: in.HostKey, Description: in.Description, Folder: in.Folder})
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, "une autre machine porte déjà ce nom dans Gná")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	event := "target.updated"
	if created {
		event = "target.created"
	}
	_ = s.st.Audit(r.Context(), nil, "liaison « "+l.Name+" »", event, s.clientIP(r),
		map[string]any{"target": in.Name, "host": in.Host, "port": in.Port, "link": l.Name, "ref": ref})
	t, err := s.st.TargetByID(r.Context(), id)
	if err != nil {
		s.internalError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"id": t.ID, "name": t.Name, "has_secret": t.HasSecret, "created": created})
}

// ---------- gestion (Paramètres › Liaisons, administrateurs) ----------

func (s *Server) handleListLinks(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Links(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// pairingCode : contenu d'un code d'appairage Runa.
type pairingCode struct {
	Outil     string `json:"outil"`
	URL       string `json:"url"`
	Empreinte string `json:"empreinte,omitempty"`
	Jeton     string `json:"jeton"`
	Cle       []byte `json:"cle,omitempty"` // clé publique de réception (secrets:write)
}

func (p pairingCode) String() string {
	b, _ := json.Marshal(p)
	return "runa1." + base64.RawURLEncoding.EncodeToString(b)
}

// pinnedFingerprint : empreinte du certificat auto-signé de Gná, que l'outil lié épinglera.
// Un certificat fourni (GNA_TLS_CERT) ou un reverse proxy est vérifié normalement.
func (s *Server) pinnedFingerprint() string {
	if !s.cfg.TLSSelfSigned {
		return ""
	}
	cert, _ := tlsutil.Paths(s.cfg.DataDir)
	info, err := tlsutil.Describe(cert)
	if err != nil {
		return ""
	}
	return info.Fingerprint
}

func (s *Server) publicURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if !decode(w, r, &req) {
		return
	}
	name := strings.Join(strings.Fields(req.Name), " ")
	if name == "" || utf8.RuneCountInString(name) > 64 {
		writeError(w, http.StatusBadRequest, "nom obligatoire (64 caractères au plus)")
		return
	}
	scopes := []string{}
	for _, sc := range req.Scopes {
		if !linkScopes[sc] {
			writeError(w, http.StatusBadRequest, "portée inconnue : "+sc)
			return
		}
		if !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}
	if len(scopes) == 0 {
		writeError(w, http.StatusBadRequest, "choisissez au moins une portée")
		return
	}
	raw, _, err := auth.NewAPIToken()
	if err != nil {
		s.internalError(w, err)
		return
	}
	token := linkTokenPrefix + strings.TrimPrefix(raw, auth.APITokenPrefix)
	user := identity(r).User
	l, err := s.st.CreateLink(r.Context(), name, auth.HashToken(token), scopes, user.Username, time.Now().Add(pairingTTL))
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "link.created", map[string]any{"link": l.Name, "link_id": l.ID, "scopes": scopes})
	code := pairingCode{Outil: "gna", URL: s.publicURL(r), Empreinte: s.pinnedFingerprint(), Jeton: token}
	if slices.Contains(scopes, ScopeSecretsWrite) {
		k, err := s.boxKey()
		if err != nil {
			s.internalError(w, err)
			return
		}
		code.Cle = k.PublicKey().Bytes()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"link": l, "code": code.String()})
}

func (s *Server) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	l, err := s.st.DeleteLink(r.Context(), pathID(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "liaison introuvable")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "link.revoked", map[string]any{"link": l.Name, "link_id": l.ID})
	w.WriteHeader(http.StatusNoContent)
}

// handleLinkedSecret reçoit d'Andvari le mot de passe d'un compte d'une machine.
func (s *Server) handleLinkedSecret(w http.ResponseWriter, r *http.Request, l *store.Link) {
	var in struct {
		Data []byte `json:"data"`
	}
	if !decode(w, r, &in) {
		return
	}
	k, err := s.boxKey()
	if err != nil {
		s.internalError(w, err)
		return
	}
	plain, err := runabox.Open(k, in.Data, []byte(secretBoxAAD))
	if err != nil {
		writeError(w, http.StatusBadRequest, "secret illisible : la clé de Gná a changé ? recréez la liaison")
		return
	}
	var sec struct {
		Host     string `json:"host"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.Unmarshal(plain, &sec) != nil || !hostRe.MatchString(sec.Host) || sec.Password == "" ||
		len(sec.Password) > 1024 || len(sec.Username) > 64 {
		writeError(w, http.StatusBadRequest, "secret incomplet (adresse, mot de passe)")
		return
	}
	targets, err := s.st.TargetsByHost(r.Context(), sec.Host)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(targets) == 0 {
		writeError(w, http.StatusNotFound, "aucune machine SSH de Gná ne correspond à "+sec.Host)
		return
	}
	box := s.auth.Box()
	updated, completed := []string{}, []string{}
	for _, t := range targets {
		sealed, err := box.Seal([]byte(sec.Password), sudoContext(t.ID))
		if err == nil {
			err = s.st.SetSudoSecret(r.Context(), t.ID, sealed)
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		updated = append(updated, t.Name)
		// Machine « à compléter » : ce compte devient son identifiant de connexion.
		if !t.HasSecret && t.AuthMethod != "none" && sec.Username != "" {
			conn, err := box.Seal([]byte(sec.Password), secretContext(t.ID))
			if err != nil {
				s.internalError(w, err)
				return
			}
			if ok, err := s.st.CompleteTarget(r.Context(), t.ID, sec.Username, conn); err != nil {
				s.internalError(w, err)
				return
			} else if ok {
				completed = append(completed, t.Name)
			}
		}
	}
	_ = s.st.Audit(r.Context(), nil, "liaison « "+l.Name+" »", "target.sudo_received", s.clientIP(r),
		map[string]any{"host": sec.Host, "username": sec.Username, "targets": updated, "completed": completed})
	writeJSON(w, http.StatusOK, map[string]any{"targets": updated, "completed": completed})
}

func sudoContext(id int64) string { return fmt.Sprintf("target:%d:sudo", id) }
