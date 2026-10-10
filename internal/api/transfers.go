package api

import (
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"time"

	"gna/internal/transfer"
)

// transferFor renvoie le dossier de transfert demandé, s'il appartient à l'utilisateur.
func (s *Server) transferFor(w http.ResponseWriter, r *http.Request) (*transfer.Session, bool) {
	if s.xfer == nil {
		writeError(w, http.StatusNotFound, "transfert de fichiers désactivé")
		return nil, false
	}
	sess, ok := s.xfer.Get(r.PathValue("tid"), identity(r).User.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "aucun transfert en cours pour cette session (la session est-elle ouverte ?)")
		return nil, false
	}
	return sess, true
}

func (s *Server) handleTransferList(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.transferFor(w, r)
	if !ok {
		return
	}
	files, used, err := s.xfer.List(sess)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if files == nil {
		files = []transfer.File{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "used": used, "max": s.xfer.MaxBytes()})
}

// handleTransferUpload : le navigateur envoie un fichier (corps brut, nom en paramètre).
func (s *Server) handleTransferUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.transferFor(w, r)
	if !ok {
		return
	}
	// Un gros fichier peut prendre plus que le délai de lecture général.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(2 * time.Hour))
	if r.ContentLength > s.xfer.MaxBytes() {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("fichier trop volumineux (maximum %d Mo par session)", s.cfg.TransferMaxMB))
		return
	}
	f, err := s.xfer.Save(sess, r.URL.Query().Get("name"), r.Body)
	switch {
	case errors.Is(err, transfer.ErrFull):
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("espace de transfert plein (maximum %d Mo par session)", s.cfg.TransferMaxMB))
		return
	case errors.Is(err, transfer.ErrBadName):
		writeError(w, http.StatusBadRequest, "nom de fichier invalide")
		return
	case err != nil:
		slog.Warn("envoi de fichier", "err", err)
		writeError(w, http.StatusBadRequest, "envoi interrompu")
		return
	}
	id := identity(r)
	_ = s.st.Audit(r.Context(), &id.User.ID, id.User.Username, "file.upload", s.clientIP(r),
		map[string]any{"target": sess.Target, "session": sess.AccessID, "file": f.Path, "size": f.Size})
	writeJSON(w, http.StatusOK, f)
}

// handleTransferDownload : récupération d'un fichier déposé dans le lecteur partagé.
func (s *Server) handleTransferDownload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.transferFor(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	f, info, err := s.xfer.OpenFile(sess, rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "fichier introuvable")
		return
	}
	defer f.Close()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Hour))

	id := identity(r)
	_ = s.st.Audit(r.Context(), &id.User.ID, id.User.Username, "file.download", s.clientIP(r),
		map[string]any{"target": sess.Target, "session": sess.AccessID, "file": rel, "size": info.Size()})

	// Contenu venant de la machine distante : jamais affiché par le navigateur.
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(rel)}))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// openTransfer prépare le dossier partagé d'une session RDP (si demandé et autorisé).
func (s *Server) openTransfer(r *http.Request, target string) *transfer.Session {
	tid := r.URL.Query().Get("t")
	if s.xfer == nil || !transfer.ValidID(tid) {
		return nil
	}
	sess, err := s.xfer.Open(tid, identity(r).User.ID, target)
	if err != nil {
		slog.Warn("dossier de transfert", "err", err)
		return nil
	}
	return sess
}
