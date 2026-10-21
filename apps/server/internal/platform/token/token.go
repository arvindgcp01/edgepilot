// Package token issues and verifies EdgePilot's access and refresh tokens.
//
// # Why this is not a JWT library
//
// The recurring vulnerability class in JWT is algorithm confusion: a library
// reads `alg` from the token and selects a verifier accordingly, so an attacker
// changes it to `none` and supplies no signature, or to `HS256` and signs with
// the public key as the HMAC secret. Every JWT library has shipped a CVE of
// this shape, and the mitigation is always "remember to pin the algorithm".
//
// This implementation does not read `alg` to decide anything. Verification
// calls ed25519.Verify unconditionally. The header field is checked for
// exact equality with "EdDSA" and the token is rejected otherwise — not to
// choose a verifier, but because a token claiming to be something else is
// evidence of an attack and should be refused loudly.
//
// That is the whole reason for ~200 lines of stdlib rather than a dependency:
// the attack surface being removed *is* the flexibility a library provides.
//
// # Two token types
//
// Access tokens are signed and stateless, so verification needs no database
// round trip. They live 15 minutes, which bounds how long a revoked session
// keeps working — the session id is in the claims, and the caller checks it
// against the revocation set.
//
// Refresh tokens are opaque random values, not JWTs. Nothing is encoded in
// them, only their SHA-256 is stored, and they are single-use. A stateless
// refresh token could not be revoked, which is the one thing a refresh token
// must support.
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors returned by verification. They are distinguished so the caller can log
// precisely; the client is told only that authentication failed.
var (
	ErrMalformed     = errors.New("token is malformed")
	ErrBadAlgorithm  = errors.New("token declares an unexpected algorithm")
	ErrBadSignature  = errors.New("token signature is invalid")
	ErrExpired       = errors.New("token has expired")
	ErrNotYetValid   = errors.New("token is not yet valid")
	ErrWrongIssuer   = errors.New("token has the wrong issuer")
	ErrWrongAudience = errors.New("token has the wrong audience")
	ErrMissingClaim  = errors.New("token is missing a required claim")
)

// Algorithm is the only algorithm this package will emit or accept.
const Algorithm = "EdDSA"

// Leeway absorbs clock skew between replicas.
//
// Thirty seconds, not five minutes: a generous window extends the life of a
// token that should already be dead, and replicas share an NTP source.
const Leeway = 30 * time.Second

// header is the JOSE header. It is written, never negotiated.
type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid,omitempty"`
}

// Claims is the access-token payload.
//
// Permissions are embedded rather than looked up per request. Resolving them
// once at login and carrying them means an authorization check is a map lookup
// rather than a join — and it is why the access token is short-lived: a
// permission revoked mid-session takes effect at the next refresh, and fifteen
// minutes is the bound on that.
type Claims struct {
	Subject   uuid.UUID `json:"sub"`
	OrgID     uuid.UUID `json:"org"`
	SessionID uuid.UUID `json:"sid"`
	TokenID   uuid.UUID `json:"jti"`
	Email     string    `json:"email,omitempty"`

	Roles       []string `json:"roles,omitempty"`
	Permissions []string `json:"perms,omitempty"`

	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ExpiresAt int64  `json:"exp"`
}

// Expiry returns the expiry as a time.
func (c Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0).UTC() }

// Issuer signs and verifies access tokens.
type Issuer struct {
	private  ed25519.PrivateKey
	public   ed25519.PublicKey
	issuer   string
	audience string
	ttl      time.Duration
	keyID    string
	now      func() time.Time
}

// Options configures an Issuer.
type Options struct {
	PrivateKey ed25519.PrivateKey
	// PublicKey is derived from PrivateKey when omitted. Supply it alone to
	// build a verify-only Issuer, which is what a service that validates tokens
	// but never mints them should hold.
	PublicKey ed25519.PublicKey
	Issuer    string
	Audience  string
	TTL       time.Duration
	// KeyID is published in the header so a future rotation can identify which
	// key signed a token. It is not consulted during verification today.
	KeyID string
	// Now is injectable for tests. Time-dependent behaviour tested with
	// time.Sleep is slow and flaky; injecting the clock makes expiry assertions
	// exact.
	Now func() time.Time
}

// NewIssuer builds an Issuer.
func NewIssuer(opts Options) (*Issuer, error) {
	if opts.PrivateKey == nil && opts.PublicKey == nil {
		return nil, errors.New("token: a private or public key is required")
	}
	if opts.PrivateKey != nil && len(opts.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("token: private key is %d bytes, want %d",
			len(opts.PrivateKey), ed25519.PrivateKeySize)
	}

	pub := opts.PublicKey
	if pub == nil {
		var ok bool
		pub, ok = opts.PrivateKey.Public().(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("token: could not derive the public key")
		}
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("token: public key is %d bytes, want %d",
			len(pub), ed25519.PublicKeySize)
	}

	if opts.Issuer == "" {
		opts.Issuer = "edgepilot"
	}
	if opts.Audience == "" {
		opts.Audience = "edgepilot-api"
	}
	if opts.TTL <= 0 {
		opts.TTL = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Issuer{
		private:  opts.PrivateKey,
		public:   pub,
		issuer:   opts.Issuer,
		audience: opts.Audience,
		ttl:      opts.TTL,
		keyID:    opts.KeyID,
		now:      opts.Now,
	}, nil
}

// PublicKey returns the verification key, for publication to other services.
func (i *Issuer) PublicKey() ed25519.PublicKey { return i.public }

// TTL returns the configured access-token lifetime.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Sign issues an access token for a principal.
func (i *Issuer) Sign(c Claims) (string, error) {
	if i.private == nil {
		return "", errors.New("token: this issuer is verify-only")
	}
	if c.Subject == uuid.Nil || c.OrgID == uuid.Nil || c.SessionID == uuid.Nil {
		return "", fmt.Errorf("%w: subject, org, and session are required", ErrMissingClaim)
	}

	now := i.now().UTC()
	c.Issuer = i.issuer
	c.Audience = i.audience
	c.IssuedAt = now.Unix()
	c.NotBefore = now.Unix()
	c.ExpiresAt = now.Add(i.ttl).Unix()
	if c.TokenID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return "", fmt.Errorf("token: generate jti: %w", err)
		}
		c.TokenID = id
	}

	headerJSON, err := json.Marshal(header{Algorithm: Algorithm, Type: "JWT", KeyID: i.keyID})
	if err != nil {
		return "", fmt.Errorf("token: encode header: %w", err)
	}
	claimsJSON, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("token: encode claims: %w", err)
	}

	signingInput := encode(headerJSON) + "." + encode(claimsJSON)
	signature := ed25519.Sign(i.private, []byte(signingInput))
	return signingInput + "." + encode(signature), nil
}

// Verify checks a token and returns its claims.
//
// The order matters: the signature is checked before any claim is trusted, so
// nothing an attacker controls influences the code path until after the token
// has been proven authentic.
func (i *Issuer) Verify(raw string) (*Claims, error) {
	headerPart, claimsPart, signaturePart, ok := split3(raw)
	if !ok {
		return nil, ErrMalformed
	}

	headerJSON, err := decode(headerPart)
	if err != nil {
		return nil, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return nil, ErrMalformed
	}

	// Checked for equality, not used to select a verifier. `alg: none` and
	// `alg: HS256` both land here and are refused; neither can steer the code
	// below, which always calls ed25519.Verify.
	if h.Algorithm != Algorithm {
		return nil, fmt.Errorf("%w: %q", ErrBadAlgorithm, h.Algorithm)
	}
	if h.Type != "" && h.Type != "JWT" {
		return nil, ErrMalformed
	}

	signature, err := decode(signaturePart)
	if err != nil {
		return nil, ErrMalformed
	}
	if len(signature) != ed25519.SignatureSize {
		return nil, ErrBadSignature
	}

	signingInput := headerPart + "." + claimsPart
	if !ed25519.Verify(i.public, []byte(signingInput), signature) {
		return nil, ErrBadSignature
	}

	// Authentic from here on.
	claimsJSON, err := decode(claimsPart)
	if err != nil {
		return nil, ErrMalformed
	}
	var c Claims
	dec := json.NewDecoder(strings.NewReader(string(claimsJSON)))
	// An unexpected claim means the token was produced by something other than
	// this issuer, even though it verified. Refuse rather than ignore.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, ErrMalformed
	}

	if c.Issuer != i.issuer {
		return nil, ErrWrongIssuer
	}
	if c.Audience != i.audience {
		return nil, ErrWrongAudience
	}
	if c.Subject == uuid.Nil || c.OrgID == uuid.Nil || c.SessionID == uuid.Nil {
		return nil, ErrMissingClaim
	}
	if c.ExpiresAt == 0 {
		// A token with no expiry would be valid forever. Refuse it rather than
		// substituting a default.
		return nil, fmt.Errorf("%w: exp", ErrMissingClaim)
	}

	now := i.now().UTC()
	if now.After(time.Unix(c.ExpiresAt, 0).Add(Leeway)) {
		return nil, ErrExpired
	}
	if c.NotBefore != 0 && now.Before(time.Unix(c.NotBefore, 0).Add(-Leeway)) {
		return nil, ErrNotYetValid
	}

	return &c, nil
}

// --- refresh tokens --------------------------------------------------------

// RefreshTokenPrefix marks EdgePilot refresh tokens, so one found in a log or a
// paste is immediately recognisable — and so the log redactor can match it by
// shape.
const RefreshTokenPrefix = "ep_refresh_"

// refreshEntropyBytes is the random payload size. 256 bits: these are bearer
// credentials with a 30-day life and no structure to constrain a guess.
const refreshEntropyBytes = 32

// NewRefreshToken returns a fresh opaque token and the SHA-256 to store.
//
// The plaintext is returned once and never persisted. Storing only the digest
// means a database disclosure yields nothing usable, and it is why the lookup
// is by hash rather than by value.
func NewRefreshToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, refreshEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("token: read entropy: %w", err)
	}
	plaintext = RefreshTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, sum[:], nil
}

// HashRefreshToken derives the stored digest from a presented token.
//
// SHA-256 without a work factor is correct here, unlike for a password: the
// token is 256 bits of uniform randomness, so there is no dictionary to attack
// and a slow hash would only add latency to every refresh.
func HashRefreshToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// LooksLikeRefreshToken reports whether a value has the expected shape.
//
// Used to reject obvious garbage before a database round trip. It is not a
// security check — a well-formed unknown token still fails the lookup.
func LooksLikeRefreshToken(s string) bool {
	if !strings.HasPrefix(s, RefreshTokenPrefix) {
		return false
	}
	body := s[len(RefreshTokenPrefix):]
	if len(body) != base64.RawURLEncoding.EncodedLen(refreshEntropyBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(body)
	return err == nil
}

// --- enrollment tokens -----------------------------------------------------

// EnrollmentTokenPrefix marks device enrollment tokens.
const EnrollmentTokenPrefix = "ep_enroll_"

// NewEnrollmentToken returns a token, its SHA-256, and a non-secret prefix.
//
// The prefix is stored alongside the digest so an operator can identify a token
// in the UI without the platform retaining anything usable. It is short enough
// to be useless as a guess and long enough to disambiguate.
func NewEnrollmentToken() (plaintext string, hash []byte, prefix string, err error) {
	buf := make([]byte, refreshEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("token: read entropy: %w", err)
	}
	plaintext = EnrollmentTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, sum[:], plaintext[:len(EnrollmentTokenPrefix)+4], nil
}

// HashEnrollmentToken derives the stored digest from a presented token.
func HashEnrollmentToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// --- encoding helpers ------------------------------------------------------

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// decode uses strict decoding, so a token with non-canonical padding bits is
// rejected rather than silently normalised — two encodings of the same bytes
// would otherwise both verify.
func decode(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

// split3 splits on '.' without allocating, and requires exactly three parts.
func split3(s string) (a, b, c string, ok bool) {
	first := strings.IndexByte(s, '.')
	if first < 0 {
		return "", "", "", false
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, '.')
	if second < 0 {
		return "", "", "", false
	}
	a, b, c = s[:first], rest[:second], rest[second+1:]
	if a == "" || b == "" || c == "" {
		return "", "", "", false
	}
	// A fourth segment means it is not a JWS compact serialisation.
	if strings.IndexByte(c, '.') >= 0 {
		return "", "", "", false
	}
	return a, b, c, true
}
