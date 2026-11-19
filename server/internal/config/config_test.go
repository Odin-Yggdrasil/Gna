package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gna.env")
	content := "# commentaire\nGNA_TEST_A=un\nexport GNA_TEST_B=\"deux trois\"\nGNA_TEST_C=fichier\n"
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNA_TEST_C", "environnement")
	os.Unsetenv("GNA_TEST_A")
	os.Unsetenv("GNA_TEST_B")
	t.Cleanup(func() { os.Unsetenv("GNA_TEST_A"); os.Unsetenv("GNA_TEST_B") })

	if err := loadEnvFile(p, true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GNA_TEST_A") != "un" || os.Getenv("GNA_TEST_B") != "deux trois" {
		t.Error("valeurs du fichier mal lues")
	}
	if os.Getenv("GNA_TEST_C") != "environnement" {
		t.Error("l'environnement doit avoir priorité sur le fichier")
	}
}

func TestLoadEnvFilePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gna.env")
	os.WriteFile(p, []byte("X=1\n"), 0o644)
	os.Chmod(p, 0o644)
	if err := loadEnvFile(p, true); err == nil {
		t.Error("un fichier lisible par tous doit être refusé")
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent.env")
	if err := loadEnvFile(p, false); err != nil {
		t.Error("le fichier par défaut absent doit être ignoré")
	}
	if err := loadEnvFile(p, true); err == nil {
		t.Error("un fichier demandé explicitement doit exister")
	}
}
