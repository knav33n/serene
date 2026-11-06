package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Sealer encrypts tokens with AES-256-GCM. The broker name is authenticated
// with each token, so a ciphertext copied to another broker's row won't open.
type Sealer struct{ aead cipher.AEAD }

// NewSealer parses a base64 32-byte key.
func NewSealer(encoded string) (*Sealer, error) {
	if encoded == "" {
		return nil, errors.New("CALM_TOKEN_KEY is not set; generate one with: openssl rand -base64 32")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("CALM_TOKEN_KEY must be 32 bytes of base64; generate one with: openssl rand -base64 32")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead}, nil
}

// Seal encrypts a token for a broker.
func (s *Sealer) Seal(broker string, token Secret) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(token.Reveal()), []byte(broker)), nil
}

// Open decrypts a token stored for a broker.
func (s *Sealer) Open(broker string, sealed []byte) (Secret, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("stored token is corrupt")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(broker))
	if err != nil {
		return "", errors.New("stored token can't be decrypted: CALM_TOKEN_KEY may have changed, so log in again")
	}
	return Secret(plain), nil
}
