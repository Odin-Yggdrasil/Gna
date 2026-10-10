package api

// Transfert de fichiers des sessions SSH (SFTP).
//
// Le terminal SSH ouvert dans le navigateur reçoit un identifiant de transfert
// (paramètre « t », choisi par le navigateur). Tant que la session est ouverte, les
// requêtes /api/sftp/{t}/… parcourent et transfèrent des fichiers par la même
// connexion SSH, avec les droits du compte de la cible. Aucun fichier n'est stocké
// sur le serveur Gná : tout passe en flux.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"gna/internal/transfer"
)

type sftpSession struct {
	userID   int64
	target   string
	accessID int64
	ssh      *ssh.Client

	mu     sync.Mutex
	client *sftp.Client
}

// get ouvre le sous-système SFTP au premier usage.
func (f *sftpSession) get() (*sftp.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client != nil {
		return f.client, nil
	}
	c, err := sftp.NewClient(f.ssh, sftp.UseConcurrentReads(true), sftp.UseConcurrentWrites(true))
	if err != nil {
		return nil, errors.New("SFTP indisponible sur cette cible (sous-système sftp désactivé dans sshd ?)")
	}
	f.client = c
	return c, nil
}

func (f *sftpSession) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client != nil {
		f.client.Close()
		f.client = nil
	}
}

type sftpRegistry struct {
	mu sync.Mutex
	m  map[string]*sftpSession
}

func (r *sftpRegistry) add(id string, f *sftpSession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*sftpSession{}
	}
	if _, exists := r.m[id]; exists {
		return false
	}
	r.m[id] = f
	return true
}

func (r *sftpRegistry) remove(id string) {
	r.mu.Lock()
	f := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	if f != nil {
		f.close()
	}
}

func (r *sftpRegistry) get(id string, userID int64) (*sftpSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.m[id]
	if !ok || f.userID != userID {
		return nil, false
	}
	return f, true
}

// openSFTP rattache le transfert de fichiers à une session SSH (si demandé et autorisé).
// La fonction renvoyée le détache à la fin de la session.
func (s *Server) openSFTP(r *http.Request, client *ssh.Client, target string) (*sftpSession, func()) {
	tid := r.URL.Query().Get("t")
	if !s.cfg.FileTransfer || !transfer.ValidID(tid) {
		return nil, func() {}
	}
	f := &sftpSession{userID: identity(r).User.ID, target: target, ssh: client}
	if !s.sftp.add(tid, f) {
		return nil, func() {}
	}
	return f, func() { s.sftp.remove(tid) }
}

func (s *Server) sftpFor(w http.ResponseWriter, r *http.Request) (*sftpSession, *sftp.Client, bool) {
	f, ok := s.sftp.get(r.PathValue("tid"), identity(r).User.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "aucune session SSH ouverte pour ce transfert")
		return nil, nil, false
	}
	c, err := f.get()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return nil, nil, false
	}
	return f, c, true
}

// sftpError traduit les erreurs SFTP courantes en messages compréhensibles.
func sftpError(w http.ResponseWriter, err error) {
	var se *sftp.StatusError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "fichier ou dossier introuvable")
	case errors.Is(err, fs.ErrPermission):
		writeError(w, http.StatusForbidden, "permission refusée sur la cible")
	case errors.As(err, &se):
		writeError(w, http.StatusBadGateway, "erreur SFTP : "+se.Error())
	default:
		writeError(w, http.StatusBadGateway, "erreur SFTP : "+err.Error())
	}
}

type sftpEntry struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Link     bool      `json:"link,omitempty"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

const maxListedEntries = 5000

func (s *Server) handleSFTPList(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.sftpFor(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		wd, err := c.Getwd()
		if err != nil {
			wd = "/"
		}
		dir = wd
	}
	dir = path.Clean("/" + dir)
	infos, err := c.ReadDir(dir)
	if err != nil {
		sftpError(w, err)
		return
	}
	entries := make([]sftpEntry, 0, len(infos))
	for _, fi := range infos {
		if len(entries) >= maxListedEntries {
			break
		}
		e := sftpEntry{Name: fi.Name(), Dir: fi.IsDir(), Size: fi.Size(), Modified: fi.ModTime().UTC()}
		if fi.Mode()&os.ModeSymlink != 0 {
			e.Link = true
			// Lien vers un dossier : navigable comme un dossier.
			if st, err := c.Stat(path.Join(dir, fi.Name())); err == nil {
				e.Dir, e.Size = st.IsDir(), st.Size()
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries, "truncated": len(infos) > len(entries)})
}

func (s *Server) handleSFTPDownload(w http.ResponseWriter, r *http.Request) {
	f, c, ok := s.sftpFor(w, r)
	if !ok {
		return
	}
	p := path.Clean("/" + r.URL.Query().Get("path"))
	file, err := c.Open(p)
	if err != nil {
		sftpError(w, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		sftpError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "ce n'est pas un fichier")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(6 * time.Hour))

	id := identity(r)
	_ = s.st.Audit(r.Context(), &id.User.ID, id.User.Username, "file.download", s.clientIP(r),
		map[string]any{"target": f.target, "session": f.accessID, "file": p, "size": info.Size(), "via": "sftp"})

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (s *Server) handleSFTPUpload(w http.ResponseWriter, r *http.Request) {
	f, c, ok := s.sftpFor(w, r)
	if !ok {
		return
	}
	name, err := transfer.CleanName(r.URL.Query().Get("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "nom de fichier invalide")
		return
	}
	dst := path.Join(path.Clean("/"+r.URL.Query().Get("dir")), name)
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if r.URL.Query().Get("overwrite") != "1" {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if _, err := c.Lstat(dst); err == nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("« %s » existe déjà", name))
			return
		}
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(6 * time.Hour))
	file, err := c.OpenFile(dst, flags)
	if err != nil {
		sftpError(w, err)
		return
	}
	n, err := file.ReadFrom(r.Body)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		slog.Warn("envoi SFTP", "err", err)
		_ = c.Remove(dst) // pas de fichier tronqué sur la cible
		writeError(w, http.StatusBadGateway, "envoi interrompu")
		return
	}
	id := identity(r)
	_ = s.st.Audit(r.Context(), &id.User.ID, id.User.Username, "file.upload", s.clientIP(r),
		map[string]any{"target": f.target, "session": f.accessID, "file": dst, "size": n, "via": "sftp"})
	writeJSON(w, http.StatusOK, map[string]any{"path": dst, "size": n})
}

func (s *Server) handleSFTPMkdir(w http.ResponseWriter, r *http.Request) {
	f, c, ok := s.sftpFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Dir  string `json:"dir"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "requête invalide")
		return
	}
	name, err := transfer.CleanName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "nom de dossier invalide")
		return
	}
	p := path.Join(path.Clean("/"+req.Dir), name)
	if err := c.Mkdir(p); err != nil {
		if _, serr := c.Lstat(p); serr == nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("« %s » existe déjà", name))
			return
		}
		sftpError(w, err)
		return
	}
	id := identity(r)
	_ = s.st.Audit(r.Context(), &id.User.ID, id.User.Username, "file.mkdir", s.clientIP(r),
		map[string]any{"target": f.target, "session": f.accessID, "dir": p, "via": "sftp"})
	writeJSON(w, http.StatusOK, map[string]string{"path": p})
}
