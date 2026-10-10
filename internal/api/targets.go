package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"gna/internal/rdp"
	"gna/internal/sshproxy"
	"gna/internal/store"
	"gna/internal/vncproxy"
)

var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)

// targetJSON est la représentation d'une cible envoyée au navigateur (sans secret).
type targetJSON struct {
	*store.Target
	HostKeyFingerprint string        `json:"host_key_fingerprint"`
	Access             *store.Access `json:"access,omitempty"` // admins uniquement
	Favorite           bool          `json:"favorite"`         // favori de l'utilisateur connecté
}

func toJSON(t *store.Target) targetJSON {
	fp := sshproxy.Fingerprint(t.HostKey)
	if t.Protocol == "rdp" && t.HostKey != "" {
		fp = "SHA256 " + t.HostKey // empreinte du certificat RDP, déjà au format AA:BB:…
	}
	return targetJSON{Target: t, HostKeyFingerprint: fp}
}

var rdpFingerprintRe = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)

// handleListTargets : toutes les cibles pour un admin (avec leurs attributions),
// seulement les cibles attribuées pour les autres utilisateurs.
func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	user := identity(r).User
	var targets []*store.Target
	var err error
	if user.IsAdmin {
		targets, err = s.st.ListTargets(r.Context())
	} else {
		targets, err = s.st.ListTargetsForUser(r.Context(), user.ID)
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	var access map[int64]*store.Access
	if user.IsAdmin {
		if access, err = s.st.AllTargetAccess(r.Context()); err != nil {
			s.internalError(w, err)
			return
		}
	}
	favorites, err := s.st.Favorites(r.Context(), user.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]targetJSON, len(targets))
	for i, t := range targets {
		out[i] = toJSON(t)
		out[i].Favorite = favorites[t.ID]
		if user.IsAdmin {
			out[i].Access = access[t.ID]
			if out[i].Access == nil {
				out[i].Access = &store.Access{Users: []int64{}, Groups: []int64{}}
			}
		} else {
			out[i].HostKey = "" // inutile aux non-admins ; l'empreinte suffit
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// loadAllowedTarget charge la cible de l'URL si l'utilisateur y a droit.
// Une cible non attribuée répond « introuvable » : on ne révèle pas son existence.
func (s *Server) loadAllowedTarget(w http.ResponseWriter, r *http.Request) (*store.Target, bool) {
	user := identity(r).User
	targetID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if !user.IsAdmin {
		ok, err := s.st.CanAccessTarget(r.Context(), user.ID, targetID)
		if err != nil {
			s.internalError(w, err)
			return nil, false
		}
		if !ok {
			_ = s.st.Audit(r.Context(), &user.ID, user.Username, "access.denied", s.clientIP(r),
				map[string]any{"target_id": targetID})
			writeError(w, http.StatusNotFound, "cible introuvable")
			return nil, false
		}
	}
	t, err := s.st.TargetByID(r.Context(), targetID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "cible introuvable")
		return nil, false
	}
	if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return t, true
}

type targetInput struct {
	Name        string        `json:"name"`
	Protocol    string        `json:"protocol"`
	Host        string        `json:"host"`
	Port        int           `json:"port"`
	Username    string        `json:"username"`
	AuthMethod  string        `json:"auth_method"`
	Secret      string        `json:"secret"`     // vide en modification = inchangé
	Passphrase  string        `json:"passphrase"` // idem
	HostKey     string        `json:"host_key"`
	Description string        `json:"description"`
	Layout      string        `json:"keyboard_layout"`
	Folder      string        `json:"folder"`
	Access      *store.Access `json:"access"` // nil = attributions inchangées
}

func (in *targetInput) validate(creating bool, prev *store.Target) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
	in.HostKey = strings.TrimSpace(in.HostKey)
	in.Description = strings.TrimSpace(in.Description)
	if in.Protocol == "" {
		in.Protocol = "ssh"
	}
	if in.Port == 0 {
		in.Port = map[string]int{"ssh": 22, "vnc": 5900, "rdp": 3389}[in.Protocol]
	}
	switch {
	case len([]rune(in.Name)) < 1 || len([]rune(in.Name)) > 64:
		return errors.New("le nom doit faire entre 1 et 64 caractères")
	case in.Protocol != "ssh" && in.Protocol != "vnc" && in.Protocol != "rdp":
		return errors.New("protocole non pris en charge (SSH, RDP ou VNC)")
	case !hostRe.MatchString(in.Host):
		return errors.New("adresse invalide (nom d'hôte ou IP)")
	case in.Port < 1 || in.Port > 65535:
		return errors.New("port invalide")
	case len(in.Description) > 500:
		return errors.New("description trop longue")
	}
	// Organisation : dossier (classement des machines).
	in.Folder = strings.Join(strings.Fields(in.Folder), " ")
	if len([]rune(in.Folder)) > 64 {
		return errors.New("nom de dossier trop long (64 caractères au plus)")
	}

	if in.Protocol != "rdp" {
		in.Layout = ""
	}
	if in.Protocol == "rdp" {
		// RDP : identifiant (éventuellement DOMAINE\utilisateur), mot de passe, certificat épinglé.
		in.Passphrase = ""
		in.HostKey = strings.ToUpper(in.HostKey)
		if in.Layout == "" {
			in.Layout = "fr"
		}
		switch {
		case in.Username == "" || len(in.Username) > 128:
			return errors.New("identifiant de connexion requis (ex. Administrateur ou DOMAINE\\utilisateur)")
		case in.AuthMethod != "password":
			return errors.New("le RDP s'authentifie par mot de passe")
		case !rdpFingerprintRe.MatchString(in.HostKey):
			return errors.New("empreinte du certificat RDP manquante : utiliser « Récupérer l'empreinte »")
		case rdp.Layouts[in.Layout] == "":
			return errors.New("disposition de clavier invalide")
		}
	} else if in.Protocol == "vnc" {
		// VNC : ni identifiant, ni clé, ni clé d'hôte ; mot de passe VNC ou aucune authentification.
		in.Username, in.HostKey, in.Passphrase = "", "", ""
		switch in.AuthMethod {
		case "none":
			in.Secret = ""
			return nil
		case "password":
		default:
			return errors.New("méthode d'authentification VNC invalide")
		}
		if len(in.Secret) > vncproxy.MaxPasswordLen {
			return fmt.Errorf("le mot de passe VNC fait au maximum %d caractères (limite du protocole)", vncproxy.MaxPasswordLen)
		}
	} else {
		switch {
		case in.Username == "" || len(in.Username) > 64:
			return errors.New("identifiant de connexion requis")
		case in.AuthMethod != "password" && in.AuthMethod != "key":
			return errors.New("méthode d'authentification invalide")
		}
		if _, err := sshproxy.ParseHostKey(in.HostKey); err != nil {
			return errors.New("clé d'hôte manquante ou invalide : utiliser « Récupérer l'empreinte »")
		}
		if in.AuthMethod == "key" && in.Secret != "" {
			if err := sshproxy.CheckPrivateKey([]byte(in.Secret), []byte(in.Passphrase)); err != nil {
				return err
			}
		}
	}

	needSecret := creating || prev.Protocol != in.Protocol || prev.AuthMethod != in.AuthMethod || !prev.HasSecret
	if in.Secret == "" && needSecret {
		if in.AuthMethod == "key" {
			return errors.New("clé privée requise")
		}
		return errors.New("mot de passe requis")
	}
	if len(in.Secret) > 16<<10 {
		return errors.New("secret trop long")
	}
	return nil
}

func (s *Server) handleSaveTarget(w http.ResponseWriter, r *http.Request) {
	var in targetInput
	if !decode(w, r, &in) {
		return
	}
	var prev *store.Target
	creating := r.PathValue("id") == ""
	t := &store.Target{}
	if !creating {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err == nil {
			prev, err = s.st.TargetByID(r.Context(), id)
		}
		if err != nil {
			writeError(w, http.StatusNotFound, "cible introuvable")
			return
		}
		t.ID = prev.ID
	}
	if err := in.validate(creating, prev); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	t.Name, t.Protocol, t.Host, t.Port = in.Name, in.Protocol, in.Host, in.Port
	t.Username, t.AuthMethod, t.HostKey, t.Description = in.Username, in.AuthMethod, in.HostKey, in.Description
	t.Layout = in.Layout
	t.Folder = in.Folder

	var seal store.Sealer
	if in.AuthMethod == "none" {
		seal = func(int64) ([]byte, []byte, error) { return nil, nil, nil } // efface tout secret
	} else if in.Secret != "" {
		box := s.auth.Box()
		seal = func(id int64) ([]byte, []byte, error) {
			sec, err := box.Seal([]byte(in.Secret), secretContext(id))
			if err != nil {
				return nil, nil, err
			}
			var pass []byte
			if in.AuthMethod == "key" && in.Passphrase != "" {
				if pass, err = box.Seal([]byte(in.Passphrase), passphraseContext(id)); err != nil {
					return nil, nil, err
				}
			}
			return sec, pass, nil
		}
	}
	id, err := s.st.SaveTarget(r.Context(), t, seal)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeError(w, http.StatusConflict, "une cible porte déjà ce nom")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "cible introuvable")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	if in.Access != nil {
		if err := s.st.SetTargetAccess(r.Context(), id, *in.Access); err != nil {
			s.internalError(w, err)
			return
		}
	}
	event := "target.updated"
	if creating {
		event = "target.created"
	}
	s.auditReq(r, event, map[string]any{"target": t.Name, "host": t.Host, "port": t.Port, "secret_changed": in.Secret != "", "access": in.Access})
	saved, err := s.st.TargetByID(r.Context(), id)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := toJSON(saved)
	if all, err := s.st.AllTargetAccess(r.Context()); err == nil {
		out.Access = all[id]
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.st.TargetByID(r.Context(), id)
	if err == nil {
		err = s.st.DeleteTarget(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "cible introuvable")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.auditReq(r, "target.deleted", map[string]any{"target": t.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleScanHostKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Host = strings.TrimSpace(in.Host)
	if in.Port == 0 {
		in.Port = 22
		if in.Protocol == "rdp" {
			in.Port = 3389
		}
	}
	if !hostRe.MatchString(in.Host) || in.Port < 1 || in.Port > 65535 {
		writeError(w, http.StatusBadRequest, "adresse ou port invalide")
		return
	}
	if in.Protocol == "rdp" {
		bin, err := rdp.Bridge(s.cfg.RDPBridge)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		fp, err := rdp.Probe(r.Context(), bin, in.Host, in.Port)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"host_key": fp, "fingerprint": "SHA256 " + fp})
		return
	}
	line, fp, err := sshproxy.ScanHostKey(r.Context(), in.Host, in.Port)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"host_key": line, "fingerprint": fp})
}

func (s *Server) handleAccessSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListAccessSessions(r.Context(), 100)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// decryptCredentials déchiffre les identifiants d'une cible juste avant la connexion.
func (s *Server) decryptCredentials(t *store.Target) (sshproxy.Credentials, error) {
	cred := sshproxy.Credentials{Username: t.Username}
	if t.AuthMethod == "none" {
		return cred, nil
	}
	if len(t.Secret) == 0 {
		return cred, errors.New("aucun identifiant enregistré pour cette cible")
	}
	box := s.auth.Box()
	sec, err := box.Open(t.Secret, secretContext(t.ID))
	if err != nil {
		return cred, fmt.Errorf("déchiffrement des identifiants : %w", err)
	}
	if t.AuthMethod == "key" {
		cred.PrivateKey = sec
		if len(t.Passphrase) > 0 {
			if cred.Passphrase, err = box.Open(t.Passphrase, passphraseContext(t.ID)); err != nil {
				return cred, fmt.Errorf("déchiffrement de la phrase de passe : %w", err)
			}
		}
	} else {
		cred.Password = string(sec)
	}
	return cred, nil
}

func (s *Server) auditReq(r *http.Request, event string, details map[string]any) {
	id := identity(r)
	_ = s.st.Audit(r.Context(), &id.User.ID, id.User.Username, event, s.clientIP(r), details)
}

func secretContext(id int64) string     { return fmt.Sprintf("target:%d:secret", id) }
func passphraseContext(id int64) string { return fmt.Sprintf("target:%d:passphrase", id) }
