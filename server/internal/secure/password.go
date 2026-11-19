// Package secure regroupe les primitives cryptographiques de Gná.
package secure

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Paramètres Argon2id (recommandation OWASP : m ≥ 19 Mio, t ≥ 2).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // en Kio, soit 64 Mio
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

const MinPasswordLen = 12

var ErrBadHash = errors.New("format de hash invalide")

// HashPassword renvoie un hash au format PHC : $argon2id$v=19$m=…,t=…,p=…$sel$hash
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword compare un mot de passe à un hash en temps constant.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash sert à garder un temps de réponse identique quand l'utilisateur n'existe pas.
var dummyHash, _ = HashPassword("gna-dummy-password-for-timing")

// BurnTime effectue une vérification factice (protection contre l'énumération des comptes).
func BurnTime(password string) { _, _ = VerifyPassword(password, dummyHash) }

// CheckPasswordPolicy applique une politique minimale.
func CheckPasswordPolicy(password string) error {
	if len([]rune(password)) < MinPasswordLen {
		return fmt.Errorf("le mot de passe doit contenir au moins %d caractères", MinPasswordLen)
	}
	return nil
}
