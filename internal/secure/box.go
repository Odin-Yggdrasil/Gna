package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Box chiffre les données sensibles stockées en base (secrets TOTP, puis plus tard
// les identifiants des machines cibles) avec AES-256-GCM.
type Box struct{ aead cipher.AEAD }

func NewBox(masterKey []byte) (*Box, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("la clé maître doit faire 32 octets")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal chiffre plain. context lie le chiffré à son usage (ex. "totp:42") :
// un chiffré copié vers une autre ligne de la base ne se déchiffrera pas.
func (b *Box) Seal(plain []byte, context string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, ad(context)), nil
}

func (b *Box) Open(sealed []byte, context string) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("chiffré trop court")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], ad(context))
}

func ad(context string) []byte {
	h := sha256.Sum256([]byte("gna/v1/" + context))
	return h[:]
}
