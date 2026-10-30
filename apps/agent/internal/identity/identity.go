// Package identity owns what makes this Mac a specific device: its UUID, its
// private key, and the certificate the server issued for that key.
//
// # Why this is a package and not three fields
//
// Everything the agent does rests on this state. If the key is readable by a
// local user, the device's identity is stealable and every audit entry
// attributed to it becomes worthless. If the state is written non-atomically, a
// power cut mid-write leaves a device that cannot enroll (it has a UUID the
// server has seen) and cannot connect (it has no usable certificate) — a Mac
// that needs a site visit.
//
// So the rules are enforced here, once:
//
//   - The private key is generated on the device and never leaves it. The server
//     sees a CSR, which carries a public key and a proof of possession.
//   - Files are written 0600 into a 0700 directory, and permissions are checked
//     on load rather than assumed.
//   - Writes are atomic: temp file, fsync, rename, fsync the directory. A crash
//     leaves either the old state or the new one.
package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// DefaultDir is where the agent keeps its identity: /var/db/edgepilot on
// macOS, /var/lib/edgepilot elsewhere (defaults_*.go).
//
// Not /etc: this is machine-generated state, not configuration an operator
// edits, and /etc is what configuration management overwrites.

const (
	keyFile  = "device.key"
	certFile = "device.crt"
	caFile   = "ca.crt"
	metaFile = "device.json"

	// dirPerm and filePerm are checked, not just applied. A key that became
	// world-readable through a bad backup restore or a careless chmod is a
	// stolen identity waiting to happen, and it is silent unless something
	// looks.
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// ErrNotEnrolled reports that no identity exists yet, which is the normal state
// on first boot rather than a failure.
var ErrNotEnrolled = errors.New("this device is not enrolled")

// ErrInsecurePermissions reports that the key or its directory is readable
// beyond root. Refusing to load is deliberate: continuing would mean operating
// with an identity that any local user can copy.
var ErrInsecurePermissions = errors.New("identity file permissions are too permissive")

// Identity is the device's cryptographic identity plus the facts the server
// assigned to it.
type Identity struct {
	DeviceID uuid.UUID
	OrgID    uuid.UUID

	// Key is the private key. It stays in memory for the process lifetime
	// because every reconnect needs it for the TLS handshake.
	Key crypto.Signer

	// CertificatePEM is the leaf issued by the device CA. CABundlePEM is the
	// chain the agent verifies the *server* with — the agent pins this rather
	// than trusting the system roots, so a device cannot be redirected to
	// another server by a compromised trust store.
	CertificatePEM []byte
	CABundlePEM    []byte

	NotAfter time.Time

	// Approved records what the server said at enrollment. The agent still
	// re-reads it from every Welcome; this is only the value to start with.
	Approved bool

	// DevicePlaneEndpoint is where to dial. Supplied by the server at
	// enrollment so a fleet can be moved without re-imaging every Mac.
	DevicePlaneEndpoint string

	// SigningPublicKeys are the Ed25519 keys this device trusts for artifact and
	// release signatures.
	SigningPublicKeys [][]byte
}

// metadata is the JSON sidecar. The key and certificates live in their own PEM
// files so that standard tooling — openssl, security(1) — can read them during
// an incident without a parser.
type metadata struct {
	DeviceID            string   `json:"device_id"`
	OrgID               string   `json:"org_id"`
	NotAfter            string   `json:"not_after"`
	Approved            bool     `json:"approved"`
	DevicePlaneEndpoint string   `json:"device_plane_endpoint"`
	SigningPublicKeys   []string `json:"signing_public_keys,omitempty"`
	EnrolledAt          string   `json:"enrolled_at"`
}

// Store reads and writes the identity on disk.
type Store struct {
	dir string
}

// NewStore builds a Store rooted at dir. An empty dir means DefaultDir.
func NewStore(dir string) *Store {
	if dir == "" {
		dir = DefaultDir
	}
	return &Store{dir: dir}
}

// Dir returns the directory the identity lives in.
func (s *Store) Dir() string { return s.dir }

// Path returns the full path of one identity file, for logging and diagnostics.
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// Exists reports whether an identity has been written.
//
// Checks the metadata file specifically: it is written last, so its presence
// means the whole set landed. A half-written enrollment leaves a key with no
// metadata, which reads as "not enrolled" and is retried — the safe direction.
func (s *Store) Exists() bool {
	_, err := os.Stat(filepath.Join(s.dir, metaFile))
	return err == nil
}

// GenerateKey creates the device key.
//
// P-256, matching the Secure Enclave's capability so that a future move to
// hardware-backed keys is not also a change of algorithm. The CA accepts P-256,
// P-384, P-521, Ed25519, and RSA ≥ 3072 — this is the intersection of "strong"
// and "what an Apple device can hold in hardware".
func GenerateKey() (crypto.Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate device key: %w", err)
	}
	return key, nil
}

// CreateCSR builds a PKCS#10 request for the device's key.
//
// The subject carries the device UUID because the contract says CN must equal
// device_id, and because it makes a CSR readable during an incident. It is *not*
// how the server decides identity — the CA reads only the public key and the
// signature, and derives the subject from the device record. A CSR is entirely
// attacker-controlled; anything else taken from it is an escalation path.
func CreateCSR(key crypto.Signer, deviceID uuid.UUID) ([]byte, error) {
	if key == nil {
		return nil, errors.New("identity: a key is required to build a CSR")
	}
	if deviceID == uuid.Nil {
		return nil, errors.New("identity: a device id is required to build a CSR")
	}

	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: deviceID.String()},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("identity: create CSR: %w", err)
	}
	return der, nil
}

// NewDeviceID generates the device's permanent identifier.
//
// UUIDv7 so that device ids sort chronologically, which makes them usable as
// index keys on the server side without a separate created_at.
func NewDeviceID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("identity: generate device id: %w", err)
	}
	return id, nil
}

// Load reads the identity from disk.
//
// Returns ErrNotEnrolled when there is nothing to load, which the caller treats
// as "enroll now" rather than as an error.
func (s *Store) Load() (*Identity, error) {
	metaBytes, err := os.ReadFile(filepath.Join(s.dir, metaFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotEnrolled
		}
		return nil, fmt.Errorf("identity: read metadata: %w", err)
	}

	meta, err := decodeMetadata(metaBytes)
	if err != nil {
		return nil, err
	}

	keyPath := filepath.Join(s.dir, keyFile)
	if err := checkPermissions(s.dir, keyPath); err != nil {
		return nil, err
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("identity: read device key: %w", err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, err
	}

	certPEM, err := os.ReadFile(filepath.Join(s.dir, certFile))
	if err != nil {
		return nil, fmt.Errorf("identity: read device certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(s.dir, caFile))
	if err != nil {
		return nil, fmt.Errorf("identity: read CA bundle: %w", err)
	}

	id := &Identity{
		DeviceID:            meta.deviceID,
		OrgID:               meta.orgID,
		Key:                 key,
		CertificatePEM:      certPEM,
		CABundlePEM:         caPEM,
		NotAfter:            meta.notAfter,
		Approved:            meta.Approved,
		DevicePlaneEndpoint: meta.DevicePlaneEndpoint,
		SigningPublicKeys:   meta.signingKeys,
	}

	// The certificate has to match the key. They are written together, so a
	// mismatch means something replaced one of them — a restored backup, a
	// half-finished renewal — and connecting would fail at the TLS handshake
	// with a far less obvious error than this one.
	if err := id.verifyKeyMatchesCertificate(); err != nil {
		return nil, err
	}
	return id, nil
}

// Save writes the identity atomically.
//
// Order matters: key, then certificates, then metadata. Metadata is what Exists
// looks for, so it is written last and only after everything it describes is
// durable. A crash at any point leaves either no identity or a complete one.
func (s *Store) Save(id *Identity) error {
	if id == nil {
		return errors.New("identity: nothing to save")
	}
	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return fmt.Errorf("identity: create %s: %w", s.dir, err)
	}
	// MkdirAll respects the umask, so an existing directory keeps whatever mode
	// it had. Set it explicitly.
	if err := os.Chmod(s.dir, dirPerm); err != nil {
		return fmt.Errorf("identity: secure %s: %w", s.dir, err)
	}

	keyPEM, err := EncodePrivateKey(id.Key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, keyFile), keyPEM); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, certFile), id.CertificatePEM); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, caFile), id.CABundlePEM); err != nil {
		return err
	}

	meta, err := encodeMetadata(id)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, metaFile), meta)
}

// UpdateCertificate replaces the certificate after a renewal, leaving the key
// and the device id untouched.
//
// Renewal keeps the same key by design: the device's identity is the keypair,
// and rotating it would make the new certificate unverifiable against anything
// the server already knows about this device.
func (s *Store) UpdateCertificate(id *Identity, certPEM, caPEM []byte, notAfter time.Time) error {
	if id == nil {
		return errors.New("identity: nothing to update")
	}
	if len(certPEM) == 0 {
		return errors.New("identity: a renewed certificate is required")
	}

	updated := *id
	updated.CertificatePEM = certPEM
	updated.NotAfter = notAfter
	if len(caPEM) > 0 {
		updated.CABundlePEM = caPEM
	}
	if err := updated.verifyKeyMatchesCertificate(); err != nil {
		return err
	}

	if err := writeFileAtomic(filepath.Join(s.dir, certFile), updated.CertificatePEM); err != nil {
		return err
	}
	if len(caPEM) > 0 {
		if err := writeFileAtomic(filepath.Join(s.dir, caFile), updated.CABundlePEM); err != nil {
			return err
		}
	}
	meta, err := encodeMetadata(&updated)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, metaFile), meta); err != nil {
		return err
	}

	*id = updated
	return nil
}

// Wipe removes the identity.
//
// Called when the server says REASON_DEVICE_DELETED: the device was
// decommissioned, and leaving a key and certificate behind on a Mac that may be
// resold or repurposed is exactly the credential nobody remembers to clean up.
func (s *Store) Wipe() error {
	var errs []error
	for _, name := range []string{metaFile, certFile, caFile, keyFile} {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("identity: remove %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// NeedsRenewal reports whether the certificate is close enough to expiry to
// renew.
//
// Renewal happens at two thirds of the certificate's life, so a device that is
// offline for a fortnight still has a valid certificate when it returns. A
// device that renews only on the last day is a device that expires during any
// outage longer than a day.
func (id *Identity) NeedsRenewal(now time.Time, lifetime time.Duration) bool {
	if id == nil || id.NotAfter.IsZero() {
		return false
	}
	if lifetime <= 0 {
		lifetime = 90 * 24 * time.Hour
	}
	return now.After(id.NotAfter.Add(-lifetime / 3))
}

// Expired reports whether the certificate is past its validity.
//
// An expired certificate cannot complete the mTLS handshake, so there is no
// point dialling: the agent needs a renewal it can no longer authenticate for,
// and the operator needs to know.
func (id *Identity) Expired(now time.Time) bool {
	return id != nil && !id.NotAfter.IsZero() && now.After(id.NotAfter)
}

// Certificate returns the leaf, parsed.
func (id *Identity) Certificate() (*x509.Certificate, error) {
	block, _ := pem.Decode(id.CertificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("identity: device certificate is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse device certificate: %w", err)
	}
	return cert, nil
}

// verifyKeyMatchesCertificate checks that the stored certificate certifies the
// stored key.
func (id *Identity) verifyKeyMatchesCertificate() error {
	cert, err := id.Certificate()
	if err != nil {
		return err
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok {
		return errors.New("identity: certificate carries an unusable public key")
	}
	if !pub.Equal(id.Key.Public()) {
		return errors.New("identity: the stored certificate does not match the stored key")
	}
	return nil
}

// EncodePrivateKey renders a device key as PKCS#8 PEM.
//
// Exported because tls.X509KeyPair wants the encoded form, and the Identity
// holds a crypto.Signer — every other consumer wants the key itself, so the
// encoded form is produced on demand rather than kept around.
func EncodePrivateKey(key crypto.Signer) ([]byte, error) {
	if key == nil {
		return nil, errors.New("identity: no key to encode")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("identity: encode device key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseKey(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("identity: device key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Older installs, or a key placed by hand, may be SEC1.
		if ecKey, ecErr := x509.ParseECPrivateKey(block.Bytes); ecErr == nil {
			return ecKey, nil
		}
		return nil, fmt.Errorf("identity: parse device key: %w", err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("identity: device key of type %T cannot sign", parsed)
	}
	return signer, nil
}

// checkPermissions refuses to load a key any local user could read.
func checkPermissions(dir, keyPath string) error {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("identity: stat %s: %w", dir, err)
	}
	if mode := dirInfo.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: %s is %04o, expected %04o",
			ErrInsecurePermissions, dir, mode, dirPerm)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		return fmt.Errorf("identity: stat %s: %w", keyPath, err)
	}
	if mode := keyInfo.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: %s is %04o, expected %04o",
			ErrInsecurePermissions, keyPath, mode, filePerm)
	}
	return nil
}

// writeFileAtomic writes through a temp file and a rename.
//
// The fsyncs are the point. Without them a rename can be durable while the
// content it points at is not, which on a Mac that lost power mid-write leaves a
// zero-length key file — present, unreadable, and indistinguishable from
// corruption at the next boot.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("identity: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// No-op once the rename has succeeded; cleanup on every failure path.
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: secure %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("identity: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("identity: rename %s to %s: %w", tmpName, path, err)
	}
	return syncDir(dir)
}

// syncDir makes a rename durable. Without it the directory entry can be lost in
// a crash even though the file's contents were synced.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("identity: open %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("identity: sync %s: %w", dir, err)
	}
	return nil
}
