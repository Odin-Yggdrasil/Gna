package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gna/internal/secure"
)

func newBox(t *testing.T) *secure.Box {
	b, err := secure.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriteReadSearch(t *testing.T) {
	box := newBox(t)
	path := Path(t.TempDir(), 42, time.Now())
	w, err := Create(path, 42, box, Header{Width: 80, Height: 24, Title: "srv-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	w.Output([]byte("Bienvenue\r\n\x1b[32modin@srv\x1b[0m:~$ "))
	w.Input([]byte("secret-tape")) // non enregistré par défaut
	w.Output([]byte("cat /etc/pass"))
	w.Output([]byte("wd\r\nroot:x:0:0\r\n"))
	w.Resize(120, 40)
	w.Output(bytes.Repeat([]byte("ligne de remplissage\r\n"), 20000)) // plusieurs blocs
	w.Output([]byte("\x1b]0;titre\x07fin de session\r\n"))
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("root:x")) {
		t.Fatal("le fichier ne doit pas contenir de texte en clair")
	}

	cast, err := ReadAll(path, 42, box)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(cast), `{"version":2,"width":80,"height":24`) {
		t.Fatalf("en-tête inattendu : %.80s", cast)
	}
	if strings.Contains(string(cast), "secret-tape") {
		t.Error("les frappes ne doivent pas être enregistrées par défaut")
	}
	if !strings.Contains(string(cast), `"r","120x40"`) {
		t.Error("le redimensionnement doit être enregistré")
	}

	hits := Search(cast, "/etc/passwd")
	if len(hits) != 1 || !strings.Contains(hits[0].Line, "odin@srv:~$ cat /etc/passwd") {
		t.Errorf("recherche à travers plusieurs événements et couleurs : %+v", hits)
	}
	if h := Search(cast, "FIN DE SESSION"); len(h) != 1 || h[0].Line != "fin de session" {
		t.Errorf("recherche insensible à la casse et sans séquence OSC : %+v", h)
	}
	if len(Search(cast, "remplissage")) != maxHits {
		t.Error("le nombre de résultats doit être plafonné")
	}

	if _, err := ReadAll(path, 43, box); err == nil {
		t.Error("un enregistrement renommé vers un autre identifiant doit être refusé")
	}
}

func TestTamperAndTruncate(t *testing.T) {
	box := newBox(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "1.gnarec")
	w, _ := Create(path, 1, box, Header{Width: 80, Height: 24}, true)
	w.Input([]byte("ls\r"))
	w.Output(bytes.Repeat([]byte("abcdefghij\r\n"), 30000))
	w.Close()
	raw, _ := os.ReadFile(path)

	// Modification d'un octet : refus.
	bad := append([]byte{}, raw...)
	bad[len(bad)/2] ^= 1
	os.WriteFile(path, bad, 0o600)
	if _, err := ReadAll(path, 1, box); err == nil || errors.Is(err, ErrIncomplete) {
		t.Errorf("un fichier altéré doit être refusé, obtenu %v", err)
	}

	// Troncature (dernier bloc supprimé) : détectée comme incomplète.
	os.WriteFile(path, raw[:len(raw)/2], 0o600)
	if _, err := ReadAll(path, 1, box); !errors.Is(err, ErrIncomplete) {
		t.Errorf("une troncature doit être signalée, obtenu %v", err)
	}
}

func TestFlushSurvivesCrash(t *testing.T) {
	box := newBox(t)
	path := filepath.Join(t.TempDir(), "2.gnarec")
	w, _ := Create(path, 2, box, Header{Width: 80, Height: 24}, false)
	w.Output([]byte("avant le crash\r\n"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	// Pas de Close : simule un arrêt brutal du service.
	cast, err := ReadAll(path, 2, box)
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(string(cast), "avant le crash") {
		t.Fatalf("le contenu écrit avant l'arrêt doit rester lisible : %v %q", err, cast)
	}
}

func TestVNCRecording(t *testing.T) {
	box := newBox(t)
	path := filepath.Join(t.TempDir(), "9.gnarec")
	w, err := CreateVNC(path, 9, box, VNCHeader{Title: "admin@Valhalla"})
	if err != nil {
		t.Fatal(err)
	}
	w.Frame([]byte("SERVERINIT"))
	time.Sleep(20 * time.Millisecond)
	big := bytes.Repeat([]byte{1, 2, 3, 4}, 100000)
	w.Frame(big)
	w.Close()

	raw, err := ReadAll(path, 9, box)
	if err != nil {
		t.Fatal(err)
	}
	h, frames, err := ParseVNC(raw)
	if err != nil || h.Title != "admin@Valhalla" || len(frames) != 2 {
		t.Fatalf("lecture : %v %+v %d", err, h, len(frames))
	}
	if string(frames[0].Data) != "SERVERINIT" || !bytes.Equal(frames[1].Data, big) || frames[1].At < 20*time.Millisecond {
		t.Error("trames ou horodatages incorrects")
	}
	if _, _, err := ParseVNC([]byte(`{"version":2}`)); err == nil {
		t.Error("un enregistrement SSH ne doit pas être lu comme du VNC")
	}
}

func TestPathUnique(t *testing.T) {
	now := time.Now()
	a, b := Path("/r", 1, now), Path("/r", 1, now.Add(time.Microsecond))
	if a == b {
		t.Error("deux sessions de même numéro doivent avoir des fichiers distincts")
	}
}
