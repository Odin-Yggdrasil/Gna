// Package totp implémente les codes à usage unique basés sur le temps (RFC 6238),
// compatibles avec Google Authenticator, Aegis, 2FAS, Bitwarden, etc.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

const (
	Period     = 30 // secondes
	Digits     = 6
	SecretSize = 20 // 160 bits, taille recommandée pour SHA-1
	Skew       = 1  // tolérance : un pas avant ou après (dérive d'horloge)
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func GenerateSecret() ([]byte, error) {
	s := make([]byte, SecretSize)
	_, err := rand.Read(s)
	return s, err
}

// EncodeSecret renvoie le secret en base32, pour une saisie manuelle dans l'appli.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// Step renvoie le numéro de pas de temps pour t.
func Step(t time.Time) int64 { return t.Unix() / Period }

// Code calcule le code HOTP (RFC 4226) pour un pas donné.
func Code(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, v%1_000_000)
}

// Validate vérifie code à l'instant now. lastStep est le dernier pas déjà accepté
// pour ce compte : tout code de ce pas ou d'un pas antérieur est refusé (anti-rejeu).
// En cas de succès, renvoie le pas utilisé, à enregistrer comme nouveau lastStep.
func Validate(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	if len(code) != Digits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	cur := Step(now)
	var matched int64
	ok := 0
	// On parcourt toute la fenêtre sans sortir tôt pour garder un temps constant.
	for i := int64(-Skew); i <= Skew; i++ {
		s := cur + i
		eq := subtle.ConstantTimeCompare([]byte(Code(secret, s)), []byte(code))
		if eq == 1 && s > lastStep && ok == 0 {
			matched, ok = s, 1
		}
	}
	return matched, ok == 1
}

// URI construit le lien otpauth:// à encoder dans le QR code.
func URI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(Period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
