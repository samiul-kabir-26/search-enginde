package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Sealer encrypts small secrets at rest with AES-256-GCM under the platform master key
// (SECRETS_MASTER_KEY). The output is nonce || ciphertext. The tenant SecretStore adds
// per-secret data keys on top of the same primitive.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer builds a sealer from a base64 32-byte key.
func NewSealer(keyB64 string) (*Sealer, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	s, err := sealerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	return s, nil
}

func sealerFromKey(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("want 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext, binding it to aad (for example the record ID).
func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// ErrUnseal means the ciphertext was tampered with, bound to other aad, or sealed under
// another key.
var ErrUnseal = errors.New("cannot unseal")

// Open decrypts what Seal produced.
func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrUnseal
	}
	out, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrUnseal
	}
	return out, nil
}
