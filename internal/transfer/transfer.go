// Package transfer gère les dossiers de transfert de fichiers des sessions RDP.
//
// Chaque session RDP reçoit un dossier, partagé avec la machine distante comme lecteur
// « Gna » par la passerelle gna-rdp. Depuis le navigateur, l'utilisateur y dépose des
// fichiers (envoi) et récupère ceux que la machine distante y a écrits (réception).
// Le dossier est supprimé à la fin de la session.
//
// La machine distante peut écrire dans ce dossier : tout ce qui en sort est traité
// comme non fiable (pas de lien symbolique suivi, chemins vérifiés composant par
// composant, téléchargement forcé en pièce jointe).
package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

var (
	ErrNotFound = errors.New("fichier introuvable")
	ErrFull     = errors.New("espace de transfert plein")
	ErrBadName  = errors.New("nom de fichier invalide")
)

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidID indique si un identifiant de transfert (choisi par le navigateur) est valide.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Session est le dossier de transfert d'une session de bureau à distance.
type Session struct {
	ID       string
	UserID   int64
	Target   string
	AccessID int64
	Dir      string
}

// Manager tient la liste des dossiers de transfert ouverts.
type Manager struct {
	base     string
	maxBytes int64
	mu       sync.Mutex
	open     map[string]*Session
}

// NewManager prépare le dossier racine et supprime les restes d'un arrêt brutal.
func NewManager(base string, maxBytes int64) (*Manager, error) {
	if err := os.RemoveAll(base); err != nil {
		return nil, fmt.Errorf("nettoyage des transferts : %w", err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("dossier des transferts : %w", err)
	}
	return &Manager{base: base, maxBytes: maxBytes, open: map[string]*Session{}}, nil
}

// MaxBytes est la taille maximale d'un dossier de transfert.
func (m *Manager) MaxBytes() int64 { return m.maxBytes }

// Open crée le dossier de transfert d'une session.
func (m *Manager) Open(id string, userID int64, target string) (*Session, error) {
	if !ValidID(id) {
		return nil, errors.New("identifiant de transfert invalide")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.open[id]; exists {
		return nil, errors.New("identifiant de transfert déjà utilisé")
	}
	dir := filepath.Join(m.base, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Session{ID: id, UserID: userID, Target: target, Dir: dir}
	m.open[id] = s
	return s, nil
}

// Close supprime le dossier de transfert (fin de session).
func (m *Manager) Close(s *Session) {
	m.mu.Lock()
	delete(m.open, s.ID)
	m.mu.Unlock()
	_ = os.RemoveAll(s.Dir)
}

// Get renvoie le dossier de transfert s'il appartient à l'utilisateur.
func (m *Manager) Get(id string, userID int64) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.open[id]
	if !ok || s.UserID != userID {
		return nil, false
	}
	return s, true
}

// File décrit un fichier du dossier de transfert.
type File struct {
	Path     string    `json:"path"` // chemin relatif, séparé par « / »
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

const maxListed = 2000

// List renvoie les fichiers du dossier (sous-dossiers compris) et la place occupée.
func (m *Manager) List(s *Session) ([]File, int64, error) {
	var files []File
	var used int64
	err := filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // dossier modifié pendant le parcours : on ignore
		}
		if !d.Type().IsRegular() {
			return nil // dossiers parcourus, liens et fichiers spéciaux ignorés
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		used += info.Size()
		if len(files) < maxListed {
			rel, _ := filepath.Rel(s.Dir, p)
			files = append(files, File{Path: filepath.ToSlash(rel), Size: info.Size(), Modified: info.ModTime().UTC()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Modified.After(files[j].Modified) })
	return files, used, err
}

// CleanName rend un nom de fichier envoyé par le navigateur sûr et utilisable sous
// Windows comme sous Linux.
func CleanName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 127 || unicode.Is(unicode.Cf, r):
			continue
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimSpace(strings.TrimRight(b.String(), ". "))
	name = strings.TrimLeft(name, ".")
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = strings.ToValidUTF8(name[:200-len(ext)], "") + ext
	}
	if name == "" {
		return "", ErrBadName
	}
	return name, nil
}

// Save enregistre un fichier envoyé par le navigateur à la racine du dossier.
// Un fichier existant n'est jamais écrasé : « nom (2).ext » est utilisé à la place.
func (m *Manager) Save(s *Session, name string, r io.Reader) (File, error) {
	name, err := CleanName(name)
	if err != nil {
		return File{}, err
	}
	_, used, err := m.List(s)
	if err != nil {
		return File{}, err
	}
	room := m.maxBytes - used
	if room <= 0 {
		return File{}, ErrFull
	}
	// Écriture dans un fichier caché hors du dossier partagé, puis déplacement : la
	// machine distante ne voit jamais un fichier à moitié reçu.
	tmp, err := os.CreateTemp(m.base, ".envoi-*")
	if err != nil {
		return File{}, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, room+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return File{}, err
	}
	if n > room {
		return File{}, ErrFull
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		dst := filepath.Join(s.Dir, candidate)
		// Link échoue si la destination existe : pas d'écrasement, même en concurrence.
		if err := os.Link(tmp.Name(), dst); err == nil {
			return File{Path: candidate, Size: n, Modified: time.Now().UTC()}, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return File{}, err
		}
	}
	return File{}, errors.New("trop de fichiers portant ce nom")
}

// OpenFile ouvre un fichier du dossier pour le télécharger.
func (m *Manager) OpenFile(s *Session, rel string) (*os.File, os.FileInfo, error) {
	parts := strings.Split(rel, "/")
	if rel == "" || len(parts) > 32 {
		return nil, nil, ErrNotFound
	}
	p := s.Dir
	for i, c := range parts {
		if c == "" || c == "." || c == ".." || strings.ContainsRune(c, 0) {
			return nil, nil, ErrNotFound
		}
		p = filepath.Join(p, c)
		info, err := os.Lstat(p)
		if err != nil {
			return nil, nil, ErrNotFound
		}
		last := i == len(parts)-1
		if (!last && !info.IsDir()) || (last && !info.Mode().IsRegular()) {
			return nil, nil, ErrNotFound // liens symboliques et fichiers spéciaux refusés
		}
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}

// NewID tire un identifiant de transfert (utilisé par les tests et l'interface).
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
