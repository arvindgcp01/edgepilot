package token

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestIssuer(t *testing.T, now func() time.Time) (*Issuer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	iss, err := NewIssuer(Options{
		PrivateKey: priv,
		Issuer:     "edgepilot",
		Audience:   "edgepilot-api",
		TTL:        15 * time.Minute,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	return iss, pub
}

func sampleClaims() Claims {
	return Claims{
		Subject:     uuid.New(),
		OrgID:       uuid.New(),
		SessionID:   uuid.New(),
		Email:       "operator@acme.test",
		Roles:       []string{"operator"},
		Permissions: []string{"device:read", "command:execute"},
	}
}

func TestSignAndVerify(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	in := sampleClaims()
	raw, err := iss.Sign(in)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	out, err := iss.Verify(raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out.Subject != in.Subject || out.OrgID != in.OrgID || out.SessionID != in.SessionID {
		t.Errorf("identity claims did not round-trip: %+v", out)
	}
	if out.Email != in.Email {
		t.Errorf("email = %q", out.Email)
	}
	if len(out.Permissions) != 2 {
		t.Errorf("permissions = %v", out.Permissions)
	}
	if out.TokenID == uuid.Nil {
		t.Error("a jti should be generated when not supplied")
	}
	if out.ExpiresAt <= out.IssuedAt {
		t.Error("expiry must be after issuance")
	}
}

// --- the attacks that motivate this implementation -------------------------

// The `alg: none` attack: strip the signature and claim the token is unsigned.
// A library that selects its verifier from `alg` accepts this.
func TestRejectsAlgNone(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	claims := sampleClaims()
	raw, err := iss.Sign(claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, claimsPart, _, _ := split3(raw)

	for _, alg := range []string{"none", "None", "NONE", ""} {
		forgedHeader, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
		if err != nil {
			t.Fatal(err)
		}
		// Empty signature, as the attack prescribes...
		forged := encode(forgedHeader) + "." + claimsPart + "."
		if _, err := iss.Verify(forged); err == nil {
			t.Errorf("alg=%q with an empty signature was accepted", alg)
		}
		// ...and with junk, in case an empty segment is what got rejected.
		forged = encode(forgedHeader) + "." + claimsPart + "." + encode([]byte("nonsense"))
		if _, err := iss.Verify(forged); !errors.Is(err, ErrBadAlgorithm) {
			t.Errorf("alg=%q should be refused as a bad algorithm, got: %v", alg, err)
		}
	}
}

// Algorithm confusion: declare HMAC and sign with the Ed25519 *public* key as
// the shared secret. The public key is not secret, so a library that trusts
// `alg` will happily verify the attacker's own signature.
func TestRejectsAlgorithmConfusion(t *testing.T) {
	t.Parallel()
	iss, pub := newTestIssuer(t, time.Now)

	claims := sampleClaims()
	raw, err := iss.Sign(claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, claimsPart, _, _ := split3(raw)

	forgedHeader, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	signingInput := encode(forgedHeader) + "." + claimsPart

	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(signingInput))
	forged := signingInput + "." + encode(mac.Sum(nil))

	if _, err := iss.Verify(forged); !errors.Is(err, ErrBadAlgorithm) {
		t.Errorf("an HS256 token signed with the public key must be refused, got: %v", err)
	}
}

// A token signed by a different key must not verify, however well-formed.
func TestRejectsForeignSignature(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)
	attacker, _ := newTestIssuer(t, time.Now)

	raw, err := attacker.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := iss.Verify(raw); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a token from another key must be refused, got: %v", err)
	}
}

// Editing the payload — escalating permissions is the obvious move — must break
// the signature.
func TestRejectsTamperedClaims(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	raw, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	headerPart, claimsPart, sigPart, _ := split3(raw)

	claimsJSON, err := decode(claimsPart)
	if err != nil {
		t.Fatal(err)
	}
	var c Claims
	if err := json.Unmarshal(claimsJSON, &c); err != nil {
		t.Fatal(err)
	}
	c.Permissions = append(c.Permissions, "command:custom_script", "user:write")
	escalated, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	forged := headerPart + "." + encode(escalated) + "." + sigPart
	if _, err := iss.Verify(forged); !errors.Is(err, ErrBadSignature) {
		t.Errorf("escalated permissions must break the signature, got: %v", err)
	}
}

// Swapping the header alone must also break the signature — the header is
// covered by it.
func TestHeaderIsSigned(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	raw, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, claimsPart, sigPart, _ := split3(raw)

	// Same declared algorithm, different key id: passes the alg check, so only
	// the signature stands between this and acceptance.
	otherHeader, err := json.Marshal(header{Algorithm: Algorithm, Type: "JWT", KeyID: "attacker"})
	if err != nil {
		t.Fatal(err)
	}
	forged := encode(otherHeader) + "." + claimsPart + "." + sigPart
	if _, err := iss.Verify(forged); !errors.Is(err, ErrBadSignature) {
		t.Errorf("the header must be covered by the signature, got: %v", err)
	}
}

func TestRejectsMalformedTokens(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	valid, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	h, c, s, _ := split3(valid)

	for name, raw := range map[string]string{
		"empty":             "",
		"no dots":           "notatoken",
		"two segments":      h + "." + c,
		"four segments":     h + "." + c + "." + s + "." + s,
		"empty header":      "." + c + "." + s,
		"empty claims":      h + ".." + s,
		"empty signature":   h + "." + c + ".",
		"header not base64": "!!!." + c + "." + s,
		"claims not base64": h + ".!!!." + s,
		"header not json":   encode([]byte("not json")) + "." + c + "." + s,
		"short signature":   h + "." + c + "." + encode([]byte("tooshort")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := iss.Verify(raw); err == nil {
				t.Error("a malformed token must not verify")
			}
		})
	}
}

// A base64 segment with non-canonical padding bits decodes to the same bytes
// under a lenient decoder, giving two encodings of one token. Strict decoding
// refuses the alternative.
func TestRejectsNonCanonicalBase64(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	raw, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Standard base64 padding is not valid in the raw URL encoding JWS uses.
	if _, err := iss.Verify(raw + "=="); err == nil {
		t.Error("padded base64 should be refused")
	}
}

// --- claim validation ------------------------------------------------------

func TestExpiry(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	now := base
	iss, _ := newTestIssuer(t, func() time.Time { return now })

	raw, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Valid inside the window.
	now = base.Add(14 * time.Minute)
	if _, err := iss.Verify(raw); err != nil {
		t.Errorf("a token inside its window should verify: %v", err)
	}

	// Still valid inside the skew leeway, which exists so replicas with slightly
	// different clocks do not reject each other's tokens.
	now = base.Add(15*time.Minute + 20*time.Second)
	if _, err := iss.Verify(raw); err != nil {
		t.Errorf("a token within the leeway should verify: %v", err)
	}

	// Dead beyond it. This bound is what limits how long a revoked session
	// keeps working.
	now = base.Add(20 * time.Minute)
	if _, err := iss.Verify(raw); !errors.Is(err, ErrExpired) {
		t.Errorf("an expired token must be refused, got: %v", err)
	}
}

func TestNotYetValid(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	now := base
	iss, _ := newTestIssuer(t, func() time.Time { return now })

	raw, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	now = base.Add(-5 * time.Minute)
	if _, err := iss.Verify(raw); !errors.Is(err, ErrNotYetValid) {
		t.Errorf("a token from the future must be refused, got: %v", err)
	}
}

// A token with no expiry would be valid forever. Substituting a default would
// be worse than refusing it: the token was not issued by this system.
func TestRejectsTokenWithoutExpiry(t *testing.T) {
	t.Parallel()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(Options{PrivateKey: priv, Issuer: "edgepilot", Audience: "edgepilot-api"})
	if err != nil {
		t.Fatal(err)
	}
	_ = pub

	// Signed with the real key, so only the missing claim stands in the way.
	claims := map[string]any{
		"sub": uuid.New().String(),
		"org": uuid.New().String(),
		"sid": uuid.New().String(),
		"jti": uuid.New().String(),
		"iss": "edgepilot",
		"aud": "edgepilot-api",
		"iat": time.Now().Unix(),
		"nbf": time.Now().Unix(),
		"exp": 0,
	}
	headerJSON, _ := json.Marshal(header{Algorithm: Algorithm, Type: "JWT"})
	claimsJSON, _ := json.Marshal(claims)
	signingInput := encode(headerJSON) + "." + encode(claimsJSON)
	sig := ed25519.Sign(priv, []byte(signingInput))

	if _, err := iss.Verify(signingInput + "." + encode(sig)); !errors.Is(err, ErrMissingClaim) {
		t.Errorf("a token with no expiry must be refused, got: %v", err)
	}
}

func TestIssuerAndAudienceAreChecked(t *testing.T) {
	t.Parallel()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mint, err := NewIssuer(Options{PrivateKey: priv, Issuer: "somewhere-else", Audience: "other-api"})
	if err != nil {
		t.Fatal(err)
	}
	check, err := NewIssuer(Options{PrivateKey: priv, Issuer: "edgepilot", Audience: "edgepilot-api"})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := mint.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Same key, so the signature is valid — only the issuer differs. Without
	// this check a token minted for another audience by the same key would be
	// accepted here.
	if _, err := check.Verify(raw); !errors.Is(err, ErrWrongIssuer) {
		t.Errorf("a token from another issuer must be refused, got: %v", err)
	}
}

func TestSignRequiresIdentityClaims(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, time.Now)

	for name, c := range map[string]Claims{
		"no subject": {OrgID: uuid.New(), SessionID: uuid.New()},
		"no org":     {Subject: uuid.New(), SessionID: uuid.New()},
		"no session": {Subject: uuid.New(), OrgID: uuid.New()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := iss.Sign(c); !errors.Is(err, ErrMissingClaim) {
				t.Errorf("expected a missing-claim error, got: %v", err)
			}
		})
	}
}

// A service that only validates tokens should not be able to mint them.
func TestVerifyOnlyIssuerCannotSign(t *testing.T) {
	t.Parallel()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewIssuer(Options{PrivateKey: priv})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewIssuer(Options{PublicKey: pub})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := signer.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := verifier.Verify(raw); err != nil {
		t.Errorf("a verify-only issuer should verify: %v", err)
	}
	if _, err := verifier.Sign(sampleClaims()); err == nil {
		t.Error("a verify-only issuer must not be able to sign")
	}
}

func TestNewIssuerRejectsBadKeys(t *testing.T) {
	t.Parallel()

	if _, err := NewIssuer(Options{}); err == nil {
		t.Error("a key is required")
	}
	if _, err := NewIssuer(Options{PrivateKey: ed25519.PrivateKey("too short")}); err == nil {
		t.Error("a malformed private key should be rejected")
	}
	if _, err := NewIssuer(Options{PublicKey: ed25519.PublicKey("too short")}); err == nil {
		t.Error("a malformed public key should be rejected")
	}
}

// --- refresh and enrollment tokens -----------------------------------------

func TestRefreshTokenShape(t *testing.T) {
	t.Parallel()

	plaintext, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if !strings.HasPrefix(plaintext, RefreshTokenPrefix) {
		t.Errorf("missing prefix: %s", plaintext)
	}
	if len(hash) != sha256.Size {
		t.Errorf("hash is %d bytes, want %d", len(hash), sha256.Size)
	}
	if !LooksLikeRefreshToken(plaintext) {
		t.Error("a freshly minted token should pass the shape check")
	}

	// The digest must be derivable from the presented token — that is how the
	// lookup works without ever storing the token.
	if string(HashRefreshToken(plaintext)) != string(hash) {
		t.Error("HashRefreshToken disagrees with the digest returned at creation")
	}

	// And the token must not be recoverable from the digest.
	if strings.Contains(string(hash), plaintext) {
		t.Error("the digest contains the token")
	}
}

func TestRefreshTokensAreUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		plaintext, _, err := NewRefreshToken()
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if _, dup := seen[plaintext]; dup {
			t.Fatal("a refresh token repeated within 1000 draws; the entropy source is broken")
		}
		seen[plaintext] = struct{}{}
	}
}

func TestLooksLikeRefreshToken(t *testing.T) {
	t.Parallel()

	valid, _, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeRefreshToken(valid) {
		t.Error("a valid token should pass")
	}
	for _, bad := range []string{
		"", "hunter2",
		"ep_refresh_",
		"ep_refresh_short",
		"ep_enroll_" + strings.Repeat("a", 43),
		valid + "extra",
		RefreshTokenPrefix + strings.Repeat("!", 43),
	} {
		if LooksLikeRefreshToken(bad) {
			t.Errorf("%q should not pass the shape check", bad)
		}
	}
}

func TestEnrollmentTokenPrefixIsNonSecret(t *testing.T) {
	t.Parallel()

	plaintext, hash, prefix, err := NewEnrollmentToken()
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if !strings.HasPrefix(plaintext, EnrollmentTokenPrefix) {
		t.Errorf("missing prefix: %s", plaintext)
	}
	if !strings.HasPrefix(plaintext, prefix) {
		t.Errorf("the stored prefix %q is not a prefix of the token", prefix)
	}
	// Short enough to be useless as a guess: it identifies a token in the UI, it
	// does not authenticate one.
	if len(prefix) != len(EnrollmentTokenPrefix)+4 {
		t.Errorf("prefix length = %d, want %d", len(prefix), len(EnrollmentTokenPrefix)+4)
	}
	if len(prefix) >= len(plaintext) {
		t.Error("the stored prefix must be a strict prefix, not the whole token")
	}
	if string(HashEnrollmentToken(plaintext)) != string(hash) {
		t.Error("HashEnrollmentToken disagrees with the digest returned at creation")
	}
}

// The signing input is exactly what RFC 7515 prescribes, so a token from this
// package is a real JWS rather than something only it can read.
func TestSigningInputMatchesJWS(t *testing.T) {
	t.Parallel()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(Options{PrivateKey: priv})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := iss.Sign(sampleClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	h, c, s, ok := split3(raw)
	if !ok {
		t.Fatal("token is not three segments")
	}

	sig, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		t.Fatalf("signature is not raw-url base64: %v", err)
	}
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), []byte(h+"."+c), sig) {
		t.Error("the signature does not cover base64(header) + '.' + base64(claims)")
	}
}

func BenchmarkVerify(b *testing.B) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	iss, _ := NewIssuer(Options{PrivateKey: priv})
	raw, _ := iss.Sign(sampleClaims())

	b.ReportAllocs()
	for b.Loop() {
		if _, err := iss.Verify(raw); err != nil {
			b.Fatal(err)
		}
	}
}
