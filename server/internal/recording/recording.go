// Package recording enregistre les sessions de terminal au format asciicast v2
// (celui d'asciinema), compressé puis chiffré sur le disque.
//
// Format du fichier .gnarec :
//
//	"GNAREC1\n" puis une suite de blocs : [longueur u32][scellé AES-GCM]
//
// Chaque bloc est chiffré avec un contexte « rec:<id>:<n>[:fin] » : on ne peut ni
// réordonner, ni supprimer, ni déplacer un bloc vers un autre enregistrement sans que
// la lecture échoue. Le dernier bloc porte la marque de fin, ce qui détecte aussi une
// troncature volontaire du fichier.
package recording

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gna/internal/secure"
)

const (
	magic     = "GNAREC1\n"
	chunkSize = 64 << 10
	maxChunk  = 1 << 20
)

// ErrIncomplete signale un enregistrement interrompu (arrêt brutal du service) :
// le contenu lisible est quand même renvoyé.
var ErrIncomplete = errors.New("enregistrement incomplet (session interrompue brutalement)")

// Path renvoie l'emplacement du fichier d'un enregistrement.
func Path(dir string, id int64, started time.Time) string {
	// L'horodatage rend le nom unique même si la numérotation des sessions repart à 1
	// (base réinitialisée en gardant le dossier des enregistrements).
	return filepath.Join(dir, started.Format("2006"), started.Format("01"),
		fmt.Sprintf("%d-%s.gnarec", id, started.UTC().Format("20060102T150405.000000000Z")))
}

// ---------- écriture ----------

// Writer enregistre une session. Ses méthodes sont sûres en accès concurrent.
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	enc     *chunkWriter
	gz      *gzip.Writer
	buf     *bufio.Writer
	start   time.Time
	input   bool
	closed  bool
	written int64
}

// Header correspond à l'en-tête asciicast v2.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Create ouvre un nouvel enregistrement. recordInput active l'enregistrement des frappes.
func Create(path string, id int64, box *secure.Box, h Header, recordInput bool) (*Writer, error) {
	w, err := open(path, id, box)
	if err != nil {
		return nil, err
	}
	w.input = recordInput
	h.Version = 2
	h.Timestamp = w.start.Unix()
	line, _ := json.Marshal(h)
	w.buf.Write(append(line, '\n'))
	return w, nil
}

// open crée le fichier chiffré et prépare la chaîne bufio → gzip → blocs chiffrés.
func open(path string, id int64, box *secure.Box) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(magic); err != nil {
		f.Close()
		return nil, err
	}
	enc := &chunkWriter{w: f, box: box, id: id}
	gz := gzip.NewWriter(enc)
	return &Writer{f: f, enc: enc, gz: gz, buf: bufio.NewWriterSize(gz, 16<<10), start: time.Now()}, nil
}

func (w *Writer) event(kind string, data string) {
	if w == nil { // enregistrement désactivé
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	ev, _ := json.Marshal([]any{roundTime(time.Since(w.start)), kind, data})
	n, _ := w.buf.Write(append(ev, '\n'))
	w.written += int64(n)
}

// Output enregistre ce que la cible affiche.
func (w *Writer) Output(p []byte) { w.event("o", string(bytes.ToValidUTF8(p, []byte("�")))) }

// Input enregistre ce que l'utilisateur tape (si activé).
func (w *Writer) Input(p []byte) {
	if w != nil && w.input {
		w.event("i", string(bytes.ToValidUTF8(p, []byte("�"))))
	}
}

// Resize enregistre un changement de taille du terminal.
func (w *Writer) Resize(cols, rows int) { w.event("r", fmt.Sprintf("%dx%d", cols, rows)) }

// Marker ajoute un repère (ex. motif de fin de session).
func (w *Writer) Marker(label string) { w.event("m", label) }

// Close termine l'enregistrement et renvoie la taille du fichier.
func (w *Writer) Close() (int64, error) {
	if w == nil {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, nil
	}
	w.closed = true
	err := errors.Join(w.buf.Flush(), w.gz.Close(), w.enc.finish())
	info, statErr := w.f.Stat()
	err = errors.Join(err, w.f.Close())
	if statErr != nil {
		return 0, err
	}
	return info.Size(), err
}

// Flush force l'écriture sur disque (appelé régulièrement : un arrêt brutal ne perd
// alors que les dernières secondes).
func (w *Writer) Flush() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if err := w.buf.Flush(); err != nil {
		return err
	}
	if err := w.gz.Flush(); err != nil {
		return err
	}
	return w.enc.flush(false)
}

func roundTime(d time.Duration) float64 {
	return float64(d.Microseconds()/1000) / 1000 // précision à la milliseconde
}

type chunkWriter struct {
	w    io.Writer
	box  *secure.Box
	id   int64
	n    int
	pend []byte
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	c.pend = append(c.pend, p...)
	for len(c.pend) >= chunkSize {
		if err := c.emit(c.pend[:chunkSize], false); err != nil {
			return 0, err
		}
		c.pend = c.pend[chunkSize:]
	}
	return len(p), nil
}

func (c *chunkWriter) flush(final bool) error {
	if len(c.pend) == 0 && !final {
		return nil
	}
	err := c.emit(c.pend, final)
	c.pend = nil
	return err
}

func (c *chunkWriter) finish() error { return c.flush(true) }

func (c *chunkWriter) emit(p []byte, final bool) error {
	sealed, err := c.box.Seal(p, chunkContext(c.id, c.n, final))
	if err != nil {
		return err
	}
	c.n++
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(sealed)))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = c.w.Write(sealed)
	return err
}

func chunkContext(id int64, n int, final bool) string {
	if final {
		return fmt.Sprintf("rec:%d:%d:fin", id, n)
	}
	return fmt.Sprintf("rec:%d:%d", id, n)
}

// ---------- lecture ----------

// ReadAll déchiffre et décompresse un enregistrement complet (asciicast v2).
// Si le fichier a été interrompu, renvoie le contenu lisible et ErrIncomplete.
func ReadAll(path string, id int64, box *secure.Box) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(r, head); err != nil || string(head) != magic {
		return nil, errors.New("fichier d'enregistrement invalide")
	}

	var compressed bytes.Buffer
	complete := false
	for n := 0; ; n++ {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			break // fin du fichier sans bloc final : session interrompue
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size > maxChunk {
			return nil, errors.New("bloc d'enregistrement invalide")
		}
		sealed := make([]byte, size)
		if _, err := io.ReadFull(r, sealed); err != nil {
			break
		}
		if plain, err := box.Open(sealed, chunkContext(id, n, false)); err == nil {
			compressed.Write(plain)
			continue
		}
		plain, err := box.Open(sealed, chunkContext(id, n, true))
		if err != nil {
			return nil, fmt.Errorf("bloc %d illisible : enregistrement altéré ou mauvaise clé", n)
		}
		compressed.Write(plain)
		complete = true
		if _, err := r.ReadByte(); err != io.EOF {
			return nil, errors.New("données inattendues après la fin de l'enregistrement")
		}
		break
	}

	gz, err := gzip.NewReader(&compressed)
	if err != nil {
		if !complete {
			return nil, ErrIncomplete // rien de lisible avant l'interruption
		}
		return nil, fmt.Errorf("enregistrement illisible : %w", err)
	}
	out, err := io.ReadAll(gz)
	if !complete {
		// Un flux gzip coupé renvoie une erreur après les données lisibles : on garde celles-ci.
		return out, ErrIncomplete
	}
	return out, err
}
