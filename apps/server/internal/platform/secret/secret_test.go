package secret

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func newTestSealer(t *testing.T) *Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := New(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	return s
}

func TestSealAndOpen(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)

	plaintext := []byte("JBSWY3DPEHPK3PXP")
	context := []byte("user:018f2a3b-1111-7222-8333-444455556666")

	sealed, err := s.Seal(plaintext, context)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Error("the sealed value contains the plaintext")
	}

	opened, err := s.Open(sealed, context)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Errorf("round trip changed the value: %q", opened)
	}
}

// GCM nonce reuse under one key leaks the authentication key, so the nonce must
// be random per seal — never a counter or a timestamp.
func TestNoncesAreUnique(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)

	plaintext := []byte("the same value every time")
	seen := map[string]struct{}{}
	for range 500 {
		sealed, err := s.Seal(plaintext, nil)
		if err != nil {
			t.Fatal(err)
		}
		nonce := string(sealed[:12])
		if _, dup := seen[nonce]; dup {
			t.Fatal("a GCM nonce repeated under one key; this leaks the authentication key")
		}
		seen[nonce] = struct{}{}
	}
}

// Sealing the same value twice must produce different ciphertexts, or an
// observer of the database learns which users share a secret.
func TestSealIsNonDeterministic(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)

	plaintext := []byte("identical")
	first, err := s.Seal(plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Seal(plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Error("two seals of the same value are identical")
	}
}

// Binding to context is what stops a sealed secret being copied from one user's
// row to another's and silently authenticating the wrong person.
func TestContextIsAuthenticated(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)

	plaintext := []byte("JBSWY3DPEHPK3PXP")
	alice := []byte("user:alice")
	bob := []byte("user:bob")

	sealed, err := s.Seal(plaintext, alice)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Open(sealed, bob); !errors.Is(err, ErrCiphertext) {
		t.Errorf("a value sealed for one user must not open for another, got: %v", err)
	}
	if _, err := s.Open(sealed, nil); !errors.Is(err, ErrCiphertext) {
		t.Errorf("dropping the context must not open the value, got: %v", err)
	}
	if _, err := s.Open(sealed, alice); err != nil {
		t.Errorf("the correct context should open it: %v", err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)

	sealed, err := s.Seal([]byte("JBSWY3DPEHPK3PXP"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func([]byte) []byte{
		"flip a ciphertext bit": func(b []byte) []byte {
			out := bytes.Clone(b)
			out[len(out)-20] ^= 0x01
			return out
		},
		"flip a nonce bit": func(b []byte) []byte {
			out := bytes.Clone(b)
			out[0] ^= 0x01
			return out
		},
		"flip a tag bit": func(b []byte) []byte {
			out := bytes.Clone(b)
			out[len(out)-1] ^= 0x01
			return out
		},
		"truncate":   func(b []byte) []byte { return b[:len(b)-1] },
		"empty":      func([]byte) []byte { return nil },
		"nonce only": func(b []byte) []byte { return b[:12] },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := s.Open(mutate(sealed), []byte("ctx")); !errors.Is(err, ErrCiphertext) {
				t.Errorf("tampering should be detected, got: %v", err)
			}
		})
	}
}

// A value sealed under one key must not open under another — that is the whole
// point of keeping the key out of the database.
func TestWrongKeyCannotOpen(t *testing.T) {
	t.Parallel()

	a := newTestSealer(t)
	b := newTestSealer(t)

	sealed, err := a.Seal([]byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed, nil); !errors.Is(err, ErrCiphertext) {
		t.Errorf("a different key must not open the value, got: %v", err)
	}
}

// Every failure reports the same error. Distinguishing "wrong key" from "wrong
// context" from "corrupt" would tell an attacker which part of a forgery to
// adjust.
func TestFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	a := newTestSealer(t)
	b := newTestSealer(t)
	sealed, err := a.Seal([]byte("secret"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}

	_, wrongKey := b.Open(sealed, []byte("ctx"))
	_, wrongContext := a.Open(sealed, []byte("other"))
	_, corrupt := a.Open(append(bytes.Clone(sealed), 0x00), []byte("ctx"))

	if wrongKey.Error() != wrongContext.Error() || wrongContext.Error() != corrupt.Error() {
		t.Errorf("failure modes are distinguishable: %v / %v / %v", wrongKey, wrongContext, corrupt)
	}
}

func TestKeySizeIsEnforced(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, 1, 16, 24, 31, 33, 64} {
		if _, err := New(make([]byte, size)); !errors.Is(err, ErrKeySize) {
			t.Errorf("a %d-byte key should be rejected for AES-256, got: %v", size, err)
		}
	}
	if _, err := New(make([]byte, 32)); err != nil {
		t.Errorf("a 32-byte key should be accepted: %v", err)
	}
}
