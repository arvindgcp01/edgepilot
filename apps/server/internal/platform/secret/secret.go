// Package secret seals small values at rest with AES-256-GCM.
//
// It exists for one job: TOTP secrets, and later any per-record credential that
// must survive a database backup without being usable from one. A stolen dump
// should yield ciphertext, not working second factors.
//
// The key comes from EP_DATA_KEY, which lives in Key Vault. That means database
// access alone is not sufficient to read a sealed value — an attacker needs the
// application's key material as well, which is a meaningfully higher bar than
// SELECT.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Errors returned by this package.
var (
	// ErrKeySize means the supplied key is not 32 bytes.
	ErrKeySize = errors.New("data key must be exactly 32 bytes for AES-256")

	// ErrCiphertext means the sealed value is too short, corrupt, or was sealed
	// under a different key or context. GCM cannot distinguish these, and it
	// should not try: any of them means the value must not be used.
	ErrCiphertext = errors.New("sealed value could not be opened")
)

// Sealer seals and opens values.
type Sealer struct {
	aead cipher.AEAD
}

// New builds a Sealer from a 32-byte key.
func New(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w (got %d)", ErrKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: new GCM: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext, binding it to context.
//
// context is authenticated but not encrypted. Passing the user id binds the
// ciphertext to that user, so a sealed TOTP secret copied from one row to
// another fails to open rather than silently authenticating the wrong person.
// That is the difference between encryption and *useful* encryption here.
//
// The nonce is random and prefixed to the output. GCM nonce reuse under one key
// is catastrophic — it leaks the authentication key — so it is never derived
// from anything predictable like a counter or a timestamp.
func (s *Sealer) Seal(plaintext, context []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, context), nil
}

// Open decrypts a value sealed with the same key and context.
//
// The error is deliberately uniform: a wrong key, a wrong context, a truncated
// value, and a tampered tag all report ErrCiphertext. Distinguishing them would
// tell an attacker which part of their forgery to adjust.
func (s *Sealer) Open(sealed, context []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	if len(sealed) < nonceSize+s.aead.Overhead() {
		return nil, ErrCiphertext
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]

	plaintext, err := s.aead.Open(nil, nonce, ciphertext, context)
	if err != nil {
		return nil, ErrCiphertext
	}
	return plaintext, nil
}
