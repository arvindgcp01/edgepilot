// Package totp implements RFC 6238 time-based one-time passwords.
//
// Implemented rather than imported because it is ~100 lines of stdlib against a
// specification with published test vectors, and correctness is therefore
// provable rather than assumed — see totp_test.go, which checks every vector in
// RFC 6238 Appendix B.
//
// HMAC-SHA1 is the algorithm, and that is not an oversight. SHA-1's collision
// weaknesses do not apply to HMAC, and every authenticator app in circulation —
// Google Authenticator, 1Password, Authy, iOS Passwords — implements SHA-1 only.
// Choosing SHA-256 here would produce codes no operator could generate.
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
	"strings"
	"time"
)

const (
	// Period is the code lifetime. Thirty seconds is what authenticator apps
	// assume; it is not configurable for that reason.
	Period = 30 * time.Second

	// Digits in a generated code.
	Digits = 6

	// SecretBytes is the shared-secret size. RFC 4226 requires at least 128
	// bits and recommends 160, which is what SHA-1's block structure uses.
	SecretBytes = 20
)

// DefaultSkew is how many periods either side of now are accepted.
//
// One period each way, so a code remains usable for up to 90 seconds. That
// covers ordinary clock drift and the case where someone starts typing at
// second 29. Widening it to accommodate a badly-set clock would triple the
// window an intercepted code stays valid in, which is the wrong trade.
const DefaultSkew = 1

// GenerateSecret returns a new random shared secret, base32-encoded.
//
// Base32 without padding is what authenticator apps expect when a secret is
// typed or scanned.
func GenerateSecret() (string, error) {
	buf := make([]byte, SecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("totp: read entropy: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// Code computes the code for a secret at a point in time.
func Code(secret string, at time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return codeForCounter(key, uint64(at.UTC().Unix())/uint64(Period.Seconds())), nil
}

// Verify reports whether code is valid for secret at time `at`.
//
// The comparison is constant-time. A byte-by-byte comparison of a six-digit
// code leaks how many leading digits were right, which reduces a 10^6 search to
// roughly 60 guesses.
//
// It returns the counter the code matched. The caller must record that value
// and refuse it again: TOTP codes are valid for a whole period, so without
// replay tracking an intercepted code can be used repeatedly for 30 seconds.
func Verify(secret, code string, at time.Time, skew int) (counter uint64, ok bool) {
	key, err := decodeSecret(secret)
	if err != nil {
		return 0, false
	}
	if len(code) != Digits {
		return 0, false
	}
	if skew < 0 {
		skew = 0
	}

	center := uint64(at.UTC().Unix()) / uint64(Period.Seconds())

	// Every candidate is checked even after a match, so the time taken does not
	// reveal *which* period matched — that would disclose the client's clock
	// offset, which is a small but free leak.
	var (
		matched      bool
		matchCounter uint64
	)
	for delta := -skew; delta <= skew; delta++ {
		c := center
		switch {
		case delta < 0:
			d := uint64(-delta)
			if d > c {
				continue
			}
			c -= d
		case delta > 0:
			c += uint64(delta)
		}

		if subtle.ConstantTimeCompare([]byte(codeForCounter(key, c)), []byte(code)) == 1 {
			matched = true
			matchCounter = c
		}
	}
	return matchCounter, matched
}

// CounterAt returns the TOTP counter for a time, so a caller can store the last
// counter it accepted and reject anything at or below it.
func CounterAt(at time.Time) uint64 {
	return uint64(at.UTC().Unix()) / uint64(Period.Seconds())
}

// ProvisioningURI builds the otpauth:// URI an authenticator app scans.
//
// The issuer appears twice — as a label prefix and as a parameter — because
// older apps read one and newer ones the other, and an app that reads neither
// shows an unlabelled entry the user cannot identify.
func ProvisioningURI(secret, accountName, issuer string) string {
	label := accountName
	if issuer != "" {
		label = issuer + ":" + accountName
	}
	q := url.Values{}
	q.Set("secret", secret)
	if issuer != "" {
		q.Set("issuer", issuer)
	}
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(int(Period.Seconds())))

	return "otpauth://totp/" + url.PathEscape(label) + "?" + q.Encode()
}

// codeForCounter is HOTP: RFC 4226 §5.3.
func codeForCounter(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation: the low nibble of the last byte selects where in the
	// digest to read from, so the code depends on the whole MAC rather than a
	// fixed slice of it.
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	return fmt.Sprintf("%0*d", Digits, value%pow10(Digits))
}

func pow10(n int) uint32 {
	out := uint32(1)
	for range n {
		out *= 10
	}
	return out
}

// decodeSecret parses a base32 secret, tolerating the formatting people apply
// by hand: lowercase, spaces, and padding all appear in secrets copied out of
// one app and into another.
func decodeSecret(secret string) ([]byte, error) {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(secret))
	normalized = strings.TrimRight(normalized, "=")
	if normalized == "" {
		return nil, fmt.Errorf("totp: secret is empty")
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized)
	if err != nil {
		return nil, fmt.Errorf("totp: secret is not valid base32: %w", err)
	}
	if len(key) < 16 {
		return nil, fmt.Errorf("totp: secret is %d bytes; RFC 4226 requires at least 16", len(key))
	}
	return key, nil
}
