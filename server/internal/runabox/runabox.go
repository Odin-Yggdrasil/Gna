// Package runabox (copie de celui de Völund) : boîte scellée de la famille Runa, pour chiffrer un secret à
// destination d'un autre outil qui seul détient la clé privée. Format v1, identique
// dans Völund, Gná et l'interface d'Andvari (clients/web/src/lib/box.ts) :
//
//	scellé = 0x01 ‖ clé publique éphémère P-256 (65 octets, non compressée) ‖ nonce (12) ‖ AES-256-GCM(texte, AAD)
//	clé    = HKDF-SHA256(secret ECDH, sel = éphémère ‖ destinataire, info « runa/box/v1 »)
//
// Une clé publique s'échange en base64 (65 octets, non compressée) ; son empreinte
// SHA-256 (AA:BB:…) figure dans le code d'appairage pour qu'un serveur intermédiaire
// ne puisse pas la remplacer.
package runabox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const (
	version  = 1
	pubLen   = 65
	nonceLen = 12
	info     = "runa/box/v1"
)

var ErrOpen = errors.New("boîte scellée illisible")

func key(shared, eph, recipient []byte) (cipher.AEAD, error) {
	salt := append(append([]byte{}, eph...), recipient...)
	k := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, salt, []byte(info)), k); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal chiffre plain pour la clé publique pub (65 octets).
func Seal(pub, plain, aad []byte) ([]byte, error) {
	recipient, err := ecdh.P256().NewPublicKey(pub)
	if err != nil {
		return nil, errors.New("clé publique du destinataire invalide")
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	ephPub := eph.PublicKey().Bytes()
	aead, err := key(shared, ephPub, pub)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	rand.Read(nonce)
	out := append([]byte{version}, ephPub...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, aad), nil
}

// Open déchiffre une boîte destinée à priv.
func Open(priv *ecdh.PrivateKey, sealed, aad []byte) ([]byte, error) {
	if len(sealed) < 1+pubLen+nonceLen+16 || sealed[0] != version {
		return nil, ErrOpen
	}
	ephPub := sealed[1 : 1+pubLen]
	eph, err := ecdh.P256().NewPublicKey(ephPub)
	if err != nil {
		return nil, ErrOpen
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return nil, ErrOpen
	}
	aead, err := key(shared, ephPub, priv.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	nonce := sealed[1+pubLen : 1+pubLen+nonceLen]
	plain, err := aead.Open(nil, nonce, sealed[1+pubLen+nonceLen:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// Fingerprint : empreinte SHA-256 d'une clé publique, au format AA:BB:….
func Fingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
