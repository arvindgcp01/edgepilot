package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// decodeMaybeBase64 accepts PEM material either literally or base64-wrapped.
//
// Both forms turn up in practice: a Kubernetes Secret or Key Vault reference
// usually yields raw PEM with real newlines, while an environment variable set
// from a CI secret or a .env file is often base64 to survive the newlines.
// Accepting both removes a class of "works locally, fails in the cluster" bugs
// that otherwise surfaces as an unhelpful parse error.
func decodeMaybeBase64(s string) string {
	s = strings.TrimSpace(s)

	// Restore escaped newlines before anything else. A value read from a .env
	// file starts with "-----BEGIN" *and* carries literal \n rather than real
	// newlines, so testing the PEM prefix first would accept it and then fail
	// to parse — which is exactly the confusing failure this function exists to
	// prevent.
	if strings.Contains(s, `\n`) {
		s = strings.ReplaceAll(s, `\n`, "\n")
	}

	if strings.HasPrefix(s, "-----BEGIN") {
		return s
	}
	if decoded, err := base64.StdEncoding.DecodeString(stripWhitespace(s)); err == nil {
		return string(decoded)
	}
	return s
}

// decodeKeyMaterial accepts a symmetric key as hex or base64.
//
// Hex is tried first and only for exactly-even, all-hex input: a 64-character
// hex string is also valid base64, and decoding it as base64 would silently
// yield 48 wrong bytes rather than the intended 32.
func decodeKeyMaterial(s string) ([]byte, error) {
	s = stripWhitespace(s)
	if isHex(s) {
		return hex.DecodeString(s)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("not valid hex or base64")
}

func isHex(s string) bool {
	if s == "" || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'f',
			r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
}
