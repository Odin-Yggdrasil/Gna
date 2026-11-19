package runabox

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func TestSealOpen(t *testing.T) {
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	pub := priv.PublicKey().Bytes()
	box, err := Seal(pub, []byte("secret"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(priv, box, []byte("aad"))
	if err != nil || string(got) != "secret" {
		t.Fatalf("ouverture : %q %v", got, err)
	}
	if _, err := Open(priv, box, []byte("autre")); err == nil {
		t.Fatal("AAD différente acceptée")
	}
	other, _ := ecdh.P256().GenerateKey(rand.Reader)
	if _, err := Open(other, box, []byte("aad")); err == nil {
		t.Fatal("ouverte avec une autre clé")
	}
	box[len(box)-1] ^= 1
	if _, err := Open(priv, box, []byte("aad")); err == nil {
		t.Fatal("boîte altérée acceptée")
	}
}

// Vecteur partagé avec Gná et l'interface d'Andvari (même format partout).
func TestVector(t *testing.T) {
	b, err := os.ReadFile("testdata/vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Private, Public, AAD, Plain, Sealed string }
	json.Unmarshal(b, &v)
	d := func(s string) []byte { x, _ := base64.StdEncoding.DecodeString(s); return x }
	priv, err := ecdh.P256().NewPrivateKey(d(v.Private))
	if err != nil || !bytes.Equal(priv.PublicKey().Bytes(), d(v.Public)) {
		t.Fatalf("clé du vecteur : %v", err)
	}
	got, err := Open(priv, d(v.Sealed), []byte(v.AAD))
	if err != nil || string(got) != v.Plain {
		t.Fatalf("vecteur : %q %v", got, err)
	}
}
