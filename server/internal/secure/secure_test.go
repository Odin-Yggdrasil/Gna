package secure

import "testing"

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword("correct horse battery staple", h); !ok {
		t.Error("le bon mot de passe doit être accepté")
	}
	if ok, _ := VerifyPassword("mauvais mot de passe", h); ok {
		t.Error("un mauvais mot de passe doit être refusé")
	}
	if _, err := VerifyPassword("x", "n'importe quoi"); err == nil {
		t.Error("un hash mal formé doit renvoyer une erreur")
	}
}

func TestBox(t *testing.T) {
	key := make([]byte, 32)
	b, _ := NewBox(key)
	sealed, err := b.Seal([]byte("secret"), "totp:1")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := b.Open(sealed, "totp:1")
	if err != nil || string(plain) != "secret" {
		t.Fatal("le déchiffrement devrait réussir")
	}
	if _, err := b.Open(sealed, "totp:2"); err == nil {
		t.Error("un chiffré déplacé vers un autre contexte ne doit pas se déchiffrer")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed, "totp:1"); err == nil {
		t.Error("un chiffré modifié doit être rejeté")
	}
}
