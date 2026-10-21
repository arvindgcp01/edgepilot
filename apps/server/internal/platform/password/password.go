// Package password hashes and verifies passwords with Argon2id.
//
// Argon2id rather than bcrypt: bcrypt silently truncates at 72 bytes, and its
// memory cost is fixed and small enough that GPU cracking is cheap. Argon2id is
// memory-hard, so the attacker's advantage from specialised hardware is bounded
// by RAM rather than by parallel compute.
//
// Two properties here matter more than the algorithm choice:
//
//   - Verification is constant-time with respect to the password. It is not
//     constant-time with respect to the *parameters*, which is fine — those are
//     public, and encoded in the hash.
//
//   - VerifyDummy exists so that an unknown account costs the same as a known
//     one. Without it, a login endpoint answers "no such user" measurably
//     faster than "wrong password", and that difference enumerates accounts.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters.
type Params struct {
	// MemoryKiB is the memory cost. This is the parameter that actually
	// frustrates GPU attacks; raising time without raising memory buys little.
	MemoryKiB uint32
	// Iterations is the time cost.
	Iterations uint32
	// Parallelism is the number of lanes.
	Parallelism uint8
	// SaltLength and KeyLength in bytes.
	SaltLength uint32
	KeyLength  uint32
	// MaxConcurrent bounds simultaneous hashing. Zero takes DefaultMaxConcurrent.
	MaxConcurrent int
}

// DefaultParams follows the second recommendation in RFC 9106: 64 MiB, t=3,
// p=4. The first recommendation (2 GiB) is not usable on a server that must
// also answer requests.
func DefaultParams() Params {
	return Params{
		MemoryKiB:   64 * 1024,
		Iterations:  3,
		Parallelism: 4,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// Errors returned by this package.
var (
	// ErrMismatch means the password does not match the hash. It is the only
	// error a caller should surface, and even then only as a generic failure.
	ErrMismatch = errors.New("password does not match")

	// ErrInvalidHash means the stored hash is not a well-formed Argon2id
	// encoding — a corrupted record or a hash from another scheme.
	ErrInvalidHash = errors.New("invalid password hash")

	// ErrIncompatibleVersion means the hash was produced by a different Argon2
	// version than this build understands.
	ErrIncompatibleVersion = errors.New("incompatible argon2 version")
)

// MinLength is the shortest password accepted.
//
// Length, not composition. Character-class rules push people toward
// "Password1!" — memorable to nobody and guessable by everybody — while adding
// almost nothing to the search space.
const MinLength = 12

// MaxLength bounds the input.
//
// Argon2 has no truncation limit, so this is not about correctness: it is about
// refusing to spend 64 MiB and three passes hashing a megabyte somebody pasted
// in to see what happens.
const MaxLength = 1024

// Hasher hashes and verifies passwords.
type Hasher struct {
	params Params
	// dummyHash is verified against when no account exists, so the work done is
	// the same either way. Computed once at construction rather than per
	// request, because computing it per request would itself be a timing
	// signal.
	dummyHash string
	// sem bounds concurrent hashing. See MaxConcurrent.
	sem chan struct{}
}

// DefaultMaxConcurrent bounds how many password hashes run at once.
//
// Memory hardness cuts both ways: at the default 64 MiB per hash, a hundred
// simultaneous login attempts would ask for 6.4 GB and take the process out.
// That is a denial of service available to anyone who can reach the login
// endpoint, and rate limiting alone does not close it — the limit is per
// account, and an attacker uses many.
//
// Queueing instead of allocating turns a memory exhaustion into added latency,
// which is the right failure mode: logins get slower under attack rather than
// the server dying.
const DefaultMaxConcurrent = 8

// New returns a Hasher. Zero-valued parameters take their defaults.
func New(p Params) (*Hasher, error) {
	d := DefaultParams()
	if p.MemoryKiB == 0 {
		p.MemoryKiB = d.MemoryKiB
	}
	if p.Iterations == 0 {
		p.Iterations = d.Iterations
	}
	if p.Parallelism == 0 {
		p.Parallelism = uint8(min(runtime.NumCPU(), int(d.Parallelism)))
	}
	if p.SaltLength == 0 {
		p.SaltLength = d.SaltLength
	}
	if p.KeyLength == 0 {
		p.KeyLength = d.KeyLength
	}
	if p.MaxConcurrent <= 0 {
		p.MaxConcurrent = DefaultMaxConcurrent
	}

	if p.MemoryKiB < 8*1024 {
		return nil, fmt.Errorf("argon2id: memory cost %d KiB is too low; use at least 8192", p.MemoryKiB)
	}
	if p.Iterations < 1 {
		return nil, errors.New("argon2id: iterations must be at least 1")
	}
	if p.SaltLength < 16 {
		return nil, fmt.Errorf("argon2id: salt length %d is too short; use at least 16", p.SaltLength)
	}
	if p.KeyLength < 16 {
		return nil, fmt.Errorf("argon2id: key length %d is too short; use at least 16", p.KeyLength)
	}

	h := &Hasher{params: p, sem: make(chan struct{}, p.MaxConcurrent)}

	// A fixed, unguessable input: the dummy hash must never accidentally match
	// a real password.
	dummy, err := h.Hash("\x00edgepilot-dummy-verification-target\x00")
	if err != nil {
		return nil, fmt.Errorf("argon2id: precompute dummy hash: %w", err)
	}
	h.dummyHash = dummy
	return h, nil
}

// Params returns the configured cost parameters.
func (h *Hasher) Params() Params { return h.params }

// Hash derives an encoded Argon2id hash.
//
// The encoding is the standard PHC string format, which carries the parameters
// and the salt. That is what lets the cost be raised later without invalidating
// every existing password: an old hash still verifies against its own
// parameters, and NeedsRehash reports that it should be upgraded.
func (h *Hasher) Hash(plaintext string) (string, error) {
	if err := ValidateLength(plaintext); err != nil {
		return "", err
	}

	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id: read salt: %w", err)
	}

	key := h.derive([]byte(plaintext), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		h.params.MemoryKiB, h.params.Iterations, h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether plaintext matches encodedHash.
//
// The comparison uses subtle.ConstantTimeCompare: a byte-by-byte comparison
// that returns early would leak how much of the derived key matched, which over
// many attempts is enough to reconstruct it.
func (h *Hasher) Verify(plaintext, encodedHash string) error {
	params, salt, want, err := decode(encodedHash)
	if err != nil {
		return err
	}

	got := h.derive([]byte(plaintext), salt,
		params.Iterations, params.MemoryKiB, params.Parallelism, uint32(len(want)))

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// VerifyDummy performs the same work as Verify against a precomputed hash, and
// always fails.
//
// Call it when no account matched, so an unknown email costs the same as a
// known one. Skipping the hash for an unknown account makes the endpoint answer
// measurably faster, and that difference is a user-enumeration oracle — which
// matters here because knowing which addresses are EdgePilot operators is
// itself useful to an attacker.
func (h *Hasher) VerifyDummy(plaintext string) {
	// The result is deliberately discarded: this exists for its cost, not its
	// answer.
	_ = h.Verify(plaintext, h.dummyHash)
}

// NeedsRehash reports whether a stored hash was produced with weaker parameters
// than the current configuration.
//
// Raising the cost only helps if existing hashes are upgraded. The caller
// re-hashes on the next successful login, where the plaintext is briefly
// available and the extra work is already expected.
func (h *Hasher) NeedsRehash(encodedHash string) bool {
	params, _, key, err := decode(encodedHash)
	if err != nil {
		// An unparseable hash certainly needs replacing.
		return true
	}
	return params.MemoryKiB < h.params.MemoryKiB ||
		params.Iterations < h.params.Iterations ||
		params.Parallelism < h.params.Parallelism ||
		uint32(len(key)) < h.params.KeyLength
}

// derive runs the KDF under the concurrency limit.
//
// Blocking here rather than in the caller keeps the bound impossible to bypass:
// every path that spends memory goes through this function.
func (h *Hasher) derive(plaintext, salt []byte, iterations, memoryKiB uint32, parallelism uint8, keyLen uint32) []byte {
	h.sem <- struct{}{}
	defer func() { <-h.sem }()
	return argon2.IDKey(plaintext, salt, iterations, memoryKiB, parallelism, keyLen)
}

// ValidateLength checks a candidate password against the accepted bounds.
func ValidateLength(plaintext string) error {
	switch {
	case len(plaintext) < MinLength:
		return fmt.Errorf("password must be at least %d characters", MinLength)
	case len(plaintext) > MaxLength:
		return fmt.Errorf("password must be at most %d characters", MaxLength)
	}
	return nil
}

// decode parses a PHC-format Argon2id hash.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		// argon2i and argon2d are deliberately refused rather than accepted:
		// only the id variant is issued here, and silently accepting another
		// would mean a hash from elsewhere could be verified against.
		return Params{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return Params{}, nil, nil, ErrIncompatibleVersion
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if p.MemoryKiB == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return Params{}, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return Params{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Params{}, nil, nil, ErrInvalidHash
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}
