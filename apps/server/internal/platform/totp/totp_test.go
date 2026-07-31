package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B publishes test vectors. Checking against them is what
// makes a hand-written implementation defensible: correctness is demonstrated,
// not asserted.
//
// The SHA-1 seed in the RFC is the ASCII string "12345678901234567890".
func rfcSecret(t *testing.T) string {
	t.Helper()
	return base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
}

func TestRFC6238Vectors(t *testing.T) {
	t.Parallel()
	secret := rfcSecret(t)

	// Appendix B, the SHA-1 rows. The RFC prints 8-digit codes; EdgePilot uses
	// 6, which is the low-order 6 digits of the same value.
	vectors := []struct {
		unix int64
		want string // last six digits of the RFC's 8-digit value
	}{
		{59, "287082"},          // 94287082
		{1111111109, "081804"},  // 07081804
		{1111111111, "050471"},  // 14050471
		{1234567890, "005924"},  // 89005924
		{2000000000, "279037"},  // 69279037
		{20000000000, "353130"}, // 65353130
	}

	for _, v := range vectors {
		at := time.Unix(v.unix, 0).UTC()
		got, err := Code(secret, at)
		if err != nil {
			t.Fatalf("code at %d: %v", v.unix, err)
		}
		if got != v.want {
			t.Errorf("code at %d = %s, want %s (RFC 6238 Appendix B)", v.unix, got, v.want)
		}
	}
}

func TestVerifyAcceptsTheCurrentCode(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	code, err := Code(secret, now)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	counter, ok := Verify(secret, code, now, DefaultSkew)
	if !ok {
		t.Fatal("the current code should verify")
	}
	if counter != CounterAt(now) {
		t.Errorf("counter = %d, want %d", counter, CounterAt(now))
	}
}

// The skew window exists for clock drift and for someone who starts typing at
// second 29. It must not extend further than that.
func TestVerifySkewWindow(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	previous, err := Code(secret, now.Add(-Period))
	if err != nil {
		t.Fatal(err)
	}
	next, err := Code(secret, now.Add(Period))
	if err != nil {
		t.Fatal(err)
	}
	tooOld, err := Code(secret, now.Add(-3*Period))
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := Verify(secret, previous, now, DefaultSkew); !ok {
		t.Error("the previous period's code should be accepted within the skew window")
	}
	if _, ok := Verify(secret, next, now, DefaultSkew); !ok {
		t.Error("the next period's code should be accepted within the skew window")
	}
	if _, ok := Verify(secret, tooOld, now, DefaultSkew); ok {
		t.Error("a code three periods old must be refused")
	}

	// With skew disabled, only the exact period counts.
	if _, ok := Verify(secret, previous, now, 0); ok {
		t.Error("with skew 0 the previous code must be refused")
	}
}

// A code is valid for a whole period, so without replay tracking an intercepted
// one works repeatedly for 30 seconds. Verify returns the counter precisely so
// the caller can refuse it a second time; this pins that it is stable.
func TestVerifyReturnsAStableCounterForReplayTracking(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	code, err := Code(secret, base)
	if err != nil {
		t.Fatal(err)
	}

	first, ok := Verify(secret, code, base, DefaultSkew)
	if !ok {
		t.Fatal("code should verify")
	}
	// Same code, later in the same period: the counter must be identical, so a
	// caller comparing against the last accepted counter rejects it.
	second, ok := Verify(secret, code, base.Add(20*time.Second), DefaultSkew)
	if !ok {
		t.Fatal("code should still verify inside its period")
	}
	if first != second {
		t.Errorf("counter changed within one period: %d then %d", first, second)
	}
}

func TestVerifyRejectsBadInput(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid, err := Code(secret, now)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ secret, code string }{
		"wrong code":       {secret, "000000"},
		"empty code":       {secret, ""},
		"too few digits":   {secret, "12345"},
		"too many digits":  {secret, "1234567"},
		"non-numeric":      {secret, "abcdef"},
		"empty secret":     {"", valid},
		"invalid base32":   {"!!!!!!!!", valid},
		"secret too short": {base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("short")), valid},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, ok := Verify(tc.secret, tc.code, now, DefaultSkew); ok {
				t.Error("should not verify")
			}
		})
	}
}

// A code from one secret must never verify against another.
func TestCodesAreSecretSpecific(t *testing.T) {
	t.Parallel()

	a, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	codeA, err := Code(a, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Verify(b, codeA, now, DefaultSkew); ok {
		t.Error("a code from one secret verified against another")
	}
}

// Secrets get copied by hand between apps, arriving lowercase, spaced, or
// padded. Rejecting those would be a support burden with no security benefit —
// the bytes are the same.
func TestSecretNormalisation(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	want, err := Code(secret, now)
	if err != nil {
		t.Fatal(err)
	}

	for name, variant := range map[string]string{
		"lowercase":  strings.ToLower(secret),
		"spaced":     strings.Join(chunk(secret, 4), " "),
		"hyphenated": strings.Join(chunk(secret, 4), "-"),
		"padded":     secret + "======",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := Code(variant, now)
			if err != nil {
				t.Fatalf("code: %v", err)
			}
			if got != want {
				t.Errorf("normalisation changed the code: %s vs %s", got, want)
			}
		})
	}
}

func TestGenerateSecretIsRandomAndLongEnough(t *testing.T) {
	t.Parallel()

	seen := map[string]struct{}{}
	for range 200 {
		s, err := GenerateSecret()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[s]; dup {
			t.Fatal("a secret repeated; the entropy source is broken")
		}
		seen[s] = struct{}{}

		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
		if err != nil {
			t.Fatalf("generated secret is not valid base32: %v", err)
		}
		// RFC 4226 requires 128 bits and recommends 160.
		if len(raw) != SecretBytes {
			t.Errorf("secret is %d bytes, want %d", len(raw), SecretBytes)
		}
	}
}

func TestProvisioningURI(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	uri := ProvisioningURI(secret, "operator@acme.test", "EdgePilot")

	for _, want := range []string{
		"otpauth://totp/",
		"secret=" + secret,
		"issuer=EdgePilot",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("provisioning URI is missing %q: %s", want, uri)
		}
	}
	// The issuer appears in the label as well as the parameter, because
	// different apps read different ones.
	if !strings.Contains(uri, "EdgePilot:operator") {
		t.Errorf("the label should carry the issuer prefix: %s", uri)
	}
}

func TestCodeIsAlwaysSixDigits(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	// A truncated value below 100000 must still render with leading zeros, or
	// the code the user sees will not match the one the server computes.
	base := time.Unix(0, 0).UTC()
	for i := range 2000 {
		code, err := Code(secret, base.Add(time.Duration(i)*Period))
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != Digits {
			t.Fatalf("code %q has %d digits, want %d", code, len(code), Digits)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q contains a non-digit", code)
			}
		}
	}
}

func chunk(s string, size int) []string {
	var out []string
	for i := 0; i < len(s); i += size {
		end := min(i+size, len(s))
		out = append(out, s[i:end])
	}
	return out
}
