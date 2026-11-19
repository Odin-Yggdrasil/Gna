package totp

import (
	"testing"
	"time"
)

// Vecteurs de la RFC 6238 (annexe B, SHA-1), tronqués à 6 chiffres.
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		if got := Code(secret, c.unix/Period); got != c.want {
			t.Errorf("t=%d : obtenu %s, attendu %s", c.unix, got, c.want)
		}
	}
}

func TestValidateWindowAndReplay(t *testing.T) {
	secret, _ := GenerateSecret()
	now := time.Unix(1_700_000_000, 0)
	cur := Step(now)

	if _, ok := Validate(secret, Code(secret, cur-1), now, 0); !ok {
		t.Error("le pas précédent devrait être accepté (dérive d'horloge)")
	}
	if _, ok := Validate(secret, Code(secret, cur-2), now, 0); ok {
		t.Error("un code de deux pas en arrière ne doit pas être accepté")
	}
	step, ok := Validate(secret, Code(secret, cur), now, 0)
	if !ok || step != cur {
		t.Fatal("le code courant devrait être accepté")
	}
	if _, ok := Validate(secret, Code(secret, cur), now, step); ok {
		t.Error("un code déjà utilisé ne doit pas être rejoué")
	}
	if _, ok := Validate(secret, "12345a", now, 0); ok {
		t.Error("un code non numérique doit être refusé")
	}
}
