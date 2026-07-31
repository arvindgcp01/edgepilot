package identity_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/agent/internal/identity"
)

// A test CA, so the identity round-trip is exercised against certificates that
// a TLS stack would actually accept rather than against arbitrary bytes.
type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "EdgePilot Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return &testCA{
		key:  key,
		cert: cert,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue signs a leaf for pub, as the real CA would for a device.
func (ca *testCA) issue(t *testing.T, pub crypto.PublicKey, deviceID uuid.UUID, notAfter time.Time) []byte {
	t.Helper()

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: deviceID.String()},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatalf("issue leaf: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// newIdentity builds a complete, valid identity in a scratch directory.
func newIdentity(t *testing.T, ca *testCA, notAfter time.Time) (*identity.Store, *identity.Identity) {
	t.Helper()

	store := identity.NewStore(t.TempDir())
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	deviceID, err := identity.NewDeviceID()
	if err != nil {
		t.Fatalf("new device id: %v", err)
	}

	id := &identity.Identity{
		DeviceID:            deviceID,
		OrgID:               uuid.New(),
		Key:                 key,
		CertificatePEM:      ca.issue(t, key.Public(), deviceID, notAfter),
		CABundlePEM:         ca.pem,
		NotAfter:            notAfter,
		Approved:            true,
		DevicePlaneEndpoint: "edgepilot.example.com:9443",
		SigningPublicKeys:   [][]byte{[]byte("signing-key-one"), []byte("signing-key-two")},
	}
	return store, id
}

// --- the round trip --------------------------------------------------------

func TestIdentitySurvivesARestart(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	store, want := newIdentity(t, ca, notAfter)

	if store.Exists() {
		t.Fatal("a fresh directory must not look enrolled")
	}
	if _, err := store.Load(); !errors.Is(err, identity.ErrNotEnrolled) {
		t.Fatalf("loading an unenrolled device should report ErrNotEnrolled, got: %v", err)
	}

	if err := store.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !store.Exists() {
		t.Fatal("Exists must be true after a save")
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got.DeviceID != want.DeviceID {
		t.Errorf("device id = %s, want %s", got.DeviceID, want.DeviceID)
	}
	if got.OrgID != want.OrgID {
		t.Errorf("org id = %s, want %s", got.OrgID, want.OrgID)
	}
	if !got.NotAfter.Equal(notAfter) {
		t.Errorf("not_after = %s, want %s", got.NotAfter, notAfter)
	}
	if !got.Approved {
		t.Error("approved was not preserved")
	}
	if got.DevicePlaneEndpoint != want.DevicePlaneEndpoint {
		t.Errorf("endpoint = %q, want %q", got.DevicePlaneEndpoint, want.DevicePlaneEndpoint)
	}
	if len(got.SigningPublicKeys) != 2 || string(got.SigningPublicKeys[0]) != "signing-key-one" {
		t.Errorf("signing keys did not round-trip: %q", got.SigningPublicKeys)
	}

	// The key must be the same key, not merely a key: the certificate on disk
	// certifies exactly one public key, and the agent has to hold its private
	// half to complete a handshake.
	loaded, ok := got.Key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok {
		t.Fatal("loaded key exposes no Equal")
	}
	if !loaded.Equal(want.Key.Public()) {
		t.Error("the loaded key is not the key that was saved")
	}
}

// --- the properties that keep the identity from being stolen ---------------

func TestTheKeyIsWrittenUnreadableToAnyoneButRoot(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	dirInfo, err := os.Stat(store.Dir())
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", mode)
	}

	for _, name := range []string{"device.key", "device.crt", "ca.crt", "device.json"} {
		info, err := os.Stat(store.Path(name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s mode = %04o, which is readable beyond root", name, mode)
		}
	}
}

// A key that became world-readable — a careless chmod, a restored backup, an
// installer run with the wrong umask — is a stolen identity waiting to happen,
// and nothing else in the system would notice.
func TestLoadRefusesAWorldReadableKey(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := os.Chmod(store.Path("device.key"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, identity.ErrInsecurePermissions) {
		t.Fatalf("loading a world-readable key should fail with ErrInsecurePermissions, got: %v", err)
	}
}

func TestLoadRefusesAWorldReadableDirectory(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := os.Chmod(store.Dir(), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.Dir(), 0o700) })

	if _, err := store.Load(); !errors.Is(err, identity.ErrInsecurePermissions) {
		t.Fatalf("a world-readable identity directory should fail with "+
			"ErrInsecurePermissions, got: %v", err)
	}
}

// A restored backup or a half-finished renewal can leave a certificate that
// certifies a different key. Connecting would fail at the TLS handshake with a
// far less obvious error than this one.
func TestLoadRefusesACertificateForADifferentKey(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	stranger := ca.issue(t, other.Public(), id.DeviceID, time.Now().Add(time.Hour))
	if err := os.WriteFile(store.Path("device.crt"), stranger, 0o600); err != nil {
		t.Fatalf("overwrite certificate: %v", err)
	}

	if _, err := store.Load(); err == nil {
		t.Fatal("a certificate that does not match the stored key must be rejected")
	}
}

// --- crash safety ----------------------------------------------------------

// Metadata is written last and is what Exists looks for, so a crash mid-save
// reads as "not enrolled" and the agent retries — rather than as a device that
// half-exists and can neither enroll nor connect.
func TestAHalfWrittenIdentityReadsAsNotEnrolled(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Simulate a crash after the key landed but before the metadata did.
	if err := os.Remove(store.Path("device.json")); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}

	if store.Exists() {
		t.Error("an identity without metadata must not look enrolled")
	}
	if _, err := store.Load(); !errors.Is(err, identity.ErrNotEnrolled) {
		t.Errorf("expected ErrNotEnrolled, got: %v", err)
	}
}

// Saving over an existing identity must never leave a partial file behind: the
// write is a temp file plus a rename.
func TestSaveLeavesNoTemporaryFilesBehind(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	for range 3 {
		if err := store.Save(id); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 4 {
		t.Errorf("directory holds %v, want exactly the four identity files", names)
	}
	for _, name := range names {
		if filepath.Ext(name) == ".tmp" || len(name) > 12 && name[:11] == "device.key." {
			t.Errorf("a temporary file was left behind: %s", name)
		}
	}
}

// --- renewal ---------------------------------------------------------------

func TestUpdateCertificateKeepsTheKeyAndTheDeviceID(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	renewedUntil := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	renewed := ca.issue(t, id.Key.Public(), id.DeviceID, renewedUntil)
	if err := store.UpdateCertificate(id, renewed, nil, renewedUntil); err != nil {
		t.Fatalf("update certificate: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.DeviceID != id.DeviceID {
		t.Errorf("renewal changed the device id: %s -> %s", id.DeviceID, got.DeviceID)
	}
	if !got.NotAfter.Equal(renewedUntil) {
		t.Errorf("not_after = %s, want %s", got.NotAfter, renewedUntil)
	}
	loaded, ok := got.Key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !loaded.Equal(id.Key.Public()) {
		t.Error("renewal must not change the device key: the identity is the keypair")
	}
}

// Renewal must refuse a certificate issued for someone else's key, which is
// what a misrouted response or a confused server would produce.
func TestUpdateCertificateRefusesAForeignCertificate(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}

	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	foreign := ca.issue(t, other.Public(), id.DeviceID, time.Now().Add(time.Hour))

	before := id.CertificatePEM
	if err := store.UpdateCertificate(id, foreign, nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a certificate for another key must be refused")
	}
	if string(id.CertificatePEM) != string(before) {
		t.Error("a refused renewal must leave the in-memory identity untouched")
	}

	// And the on-disk copy too.
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got.CertificatePEM) != string(before) {
		t.Error("a refused renewal must leave the stored certificate untouched")
	}
}

// Two thirds of the lifetime, not the last day: a device offline for a
// fortnight must still come back with a valid certificate.
func TestRenewalStartsWithPlentyOfLifeLeft(t *testing.T) {
	t.Parallel()

	const lifetime = 90 * 24 * time.Hour
	notAfter := time.Now().Add(lifetime)
	id := &identity.Identity{NotAfter: notAfter}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"freshly issued", notAfter.Add(-lifetime), false},
		{"halfway", notAfter.Add(-lifetime / 2), false},
		{"two thirds through", notAfter.Add(-lifetime/3 + time.Hour), true},
		{"a week out", notAfter.Add(-7 * 24 * time.Hour), true},
		{"expired", notAfter.Add(time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := id.NeedsRenewal(tc.at, lifetime); got != tc.want {
				t.Errorf("NeedsRenewal = %v, want %v", got, tc.want)
			}
		})
	}

	if id.Expired(notAfter.Add(-time.Hour)) {
		t.Error("a certificate before its not_after is not expired")
	}
	if !id.Expired(notAfter.Add(time.Hour)) {
		t.Error("a certificate past its not_after is expired")
	}
}

// --- decommissioning -------------------------------------------------------

// A decommissioned Mac may be resold or repurposed. Leaving a key and
// certificate on it is exactly the credential nobody remembers to clean up.
func TestWipeRemovesEverything(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	store, id := newIdentity(t, ca, time.Now().Add(time.Hour))
	if err := store.Save(id); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Wipe(); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("wipe left %d files behind", len(entries))
	}
	if store.Exists() {
		t.Error("a wiped device must not look enrolled")
	}

	// Wiping twice is what happens when the server repeats the instruction on
	// reconnect, and it must not be an error.
	if err := store.Wipe(); err != nil {
		t.Errorf("wiping an already-wiped identity should be a no-op, got: %v", err)
	}
}

// --- the CSR ---------------------------------------------------------------

func TestCSRProvesPossessionOfTheDeviceKey(t *testing.T) {
	t.Parallel()

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	deviceID, err := identity.NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}

	der, err := identity.CreateCSR(key, deviceID)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	// The signature is the only thing that matters: it is what proves the
	// requester holds the private half of the key it is asking to have
	// certified.
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CSR signature does not verify: %v", err)
	}
	if csr.Subject.CommonName != deviceID.String() {
		t.Errorf("CN = %q, want the device id %s", csr.Subject.CommonName, deviceID)
	}
	pub, ok := csr.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) {
		t.Error("the CSR does not carry the device's public key")
	}
}

func TestCSRRequiresBothAKeyAndADeviceID(t *testing.T) {
	t.Parallel()

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.CreateCSR(nil, uuid.New()); err == nil {
		t.Error("a CSR without a key must be refused")
	}
	if _, err := identity.CreateCSR(key, uuid.Nil); err == nil {
		t.Error("a CSR without a device id must be refused")
	}
}

// Device ids sort chronologically, which is what makes them usable as index
// keys on the server without a separate created_at column.
func TestDeviceIDsAreTimeOrdered(t *testing.T) {
	t.Parallel()

	first, err := identity.NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := identity.NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}

	if first.Version() != 7 {
		t.Errorf("device id version = %d, want 7", first.Version())
	}
	if first.String() >= second.String() {
		t.Errorf("device ids do not sort chronologically: %s >= %s", first, second)
	}
}

// The key must be one the server's CA will accept. P-256 is the intersection of
// "strong" and "holdable in a Secure Enclave".
func TestTheDeviceKeyIsP256(t *testing.T) {
	t.Parallel()

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := key.Public().(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("device key is %T, want an ECDSA key", key.Public())
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("curve = %s, want P-256", pub.Curve.Params().Name)
	}
}
