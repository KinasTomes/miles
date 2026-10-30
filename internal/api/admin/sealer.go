package admin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/KinasTomes/miles/internal/storage"
)

// Sealer encrypts SigV4 secrets at rest (Credentials.security). The secret
// is returned to the client once, on create.
type Sealer interface {
	Seal(ctx context.Context, accessKeyID string, secret []byte) (storage.CredentialSecurity, error)
	Open(ctx context.Context, accessKeyID string, sec storage.CredentialSecurity) ([]byte, error)
}

type aesSealer struct {
	keyID string
	aead  cipher.AEAD
}

// NewAESSealer seals with AES-256-GCM under a 32-byte key: Ciphertext is a
// random 12-byte nonce followed by the sealed secret, the access key ID is
// the additional data, Version is 1.
func NewAESSealer(keyID string, key []byte) (Sealer, error) {
	if keyID == "" || len(key) != 32 {
		return nil, errors.New("admin sealer: key id required and key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aesSealer{keyID: keyID, aead: aead}, nil
}

func (a aesSealer) Seal(_ context.Context, accessKeyID string, secret []byte) (storage.CredentialSecurity, error) {
	nonce := make([]byte, a.aead.NonceSize(), a.aead.NonceSize()+len(secret)+a.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return storage.CredentialSecurity{}, err
	}
	return storage.CredentialSecurity{Version: 1, KeyID: a.keyID, Ciphertext: a.aead.Seal(nonce, nonce, secret, []byte(accessKeyID))}, nil
}

func (a aesSealer) Open(_ context.Context, accessKeyID string, sec storage.CredentialSecurity) ([]byte, error) {
	n := a.aead.NonceSize()
	if sec.Version != 1 || sec.KeyID != a.keyID || len(sec.Ciphertext) < n+a.aead.Overhead() {
		return nil, fmt.Errorf("admin sealer: unsupported credential security of %s", accessKeyID)
	}
	secret, err := a.aead.Open(nil, sec.Ciphertext[:n], sec.Ciphertext[n:], []byte(accessKeyID))
	if err != nil {
		return nil, fmt.Errorf("admin sealer: credential security of %s does not open", accessKeyID)
	}
	return secret, nil
}
