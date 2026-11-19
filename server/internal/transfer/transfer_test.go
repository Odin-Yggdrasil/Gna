package transfer

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"rapport.pdf":         "rapport.pdf",
		"../../etc/passwd":    "passwd",
		`C:\Users\a\b.txt`:    "b.txt",
		".bashrc":             "bashrc",
		"a:b*c?.txt":          "a_b_c_.txt",
		"nom\x00caché\n.txt":  "nomcaché.txt",
		"fin. ":               "fin",
		"été 2026 — notes.md": "été 2026 — notes.md",
	}
	for in, want := range cases {
		got, err := CleanName(in)
		if err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v ; attendu %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "..", "/", "...", "  "} {
		if _, err := CleanName(bad); err == nil {
			t.Errorf("CleanName(%q) aurait dû échouer", bad)
		}
	}
}

func TestSaveListOpen(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "transfers"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	s, err := m.Open(id, 7, "cible")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(id, 8); ok {
		t.Fatal("un autre utilisateur ne doit pas accéder au dossier")
	}
	f1, err := m.Save(s, "a.txt", strings.NewReader("bonjour"))
	if err != nil || f1.Path != "a.txt" {
		t.Fatal(f1, err)
	}
	f2, err := m.Save(s, "a.txt", strings.NewReader("encore"))
	if err != nil || f2.Path != "a (2).txt" {
		t.Fatal(f2, err)
	}
	if _, err := m.Save(s, "gros.bin", strings.NewReader(strings.Repeat("x", 2000))); !errors.Is(err, ErrFull) {
		t.Fatal("quota non respecté :", err)
	}
	os.MkdirAll(filepath.Join(s.Dir, "sous"), 0o700)
	os.WriteFile(filepath.Join(s.Dir, "sous", "b.txt"), []byte("depuis la cible"), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(s.Dir, "lien"))
	files, used, err := m.List(s)
	if err != nil || len(files) != 3 || used != int64(len("bonjour")+len("encore")+len("depuis la cible")) {
		t.Fatal(files, used, err)
	}
	f, _, err := m.OpenFile(s, "sous/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "depuis la cible" {
		t.Fatal(string(b))
	}
	for _, bad := range []string{"lien", "../" + id + "/a.txt", "sous/../a.txt", "/etc/passwd", "sous", ""} {
		if f, _, err := m.OpenFile(s, bad); err == nil {
			f.Close()
			t.Errorf("OpenFile(%q) aurait dû être refusé", bad)
		}
	}
	m.Close(s)
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("dossier non supprimé")
	}
}
