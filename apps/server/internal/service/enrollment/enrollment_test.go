package enrollment_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/service/enrollment"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
)

type fixture struct {
	svc    *enrollment.Service
	store  *postgres.Store
	ca     *ca.Authority
	acme   *domain.Organization
	globex *domain.Organization
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	db := testsupport.NewMigratedDatabase(t)
	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN, MaxConns: 2, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hierarchy, err := ca.Generate(ca.GenerateOptions{Organization: "EdgePilot Test"})
	if err != nil {
		t.Fatalf("generate ca: %v", err)
	}
	authority, err := ca.New(ca.Options{
		SigningCertPEM: hierarchy.ChainPEM,
		SigningKeyPEM:  hierarchy.IntermediateKeyPEM,
	})
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}

	svc, err := enrollment.New(enrollment.Options{
		Store:               store,
		CA:                  authority,
		Logger:              slog.New(slog.DiscardHandler),
		DevicePlaneEndpoint: "edgepilot.test:9443",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	f := &fixture{svc: svc, store: store, ca: authority}
	f.acme = f.org(t, "Acme")
	f.globex = f.org(t, "Globex")
	return f
}

func (f *fixture) org(t *testing.T, name string) *domain.Organization {
	t.Helper()
	org, err := f.store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: name, Slug: strings.ToLower(name) + "-" + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return org
}

// mintToken creates an enrollment token and returns its plaintext.
func (f *fixture) mintToken(t *testing.T, org *domain.Organization, autoApprove bool, maxUses *int, labels map[string]string) string {
	t.Helper()
	plaintext, hash, prefix, err := token.NewEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnrollmentTokens().Create(
		domain.WithOrg(t.Context(), org.ID),
		domain.CreateEnrollmentTokenInput{
			OrgID:       org.ID,
			Description: "test",
			MaxUses:     maxUses,
			AutoApprove: autoApprove,
			Labels:      labels,
			ExpiresAt:   time.Now().Add(24 * time.Hour),
		}, hash, prefix); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return plaintext
}

// newCSR returns a DER certificate request and the key that signed it.
func newCSR(t *testing.T, subject pkix.Name) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: subject}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func (f *fixture) enroll(t *testing.T, tok, serial string, deviceID uuid.UUID) (*enrollment.Result, error) {
	t.Helper()
	return f.svc.Enroll(t.Context(), enrollment.Request{
		Token:        tok,
		DeviceID:     deviceID,
		CSRDer:       newCSR(t, pkix.Name{}),
		SerialNumber: serial,
		Hostname:     "mac-" + serial,
		Arch:         "arm64",
		OSVersion:    "15.3.1",
	})
}

// --- the happy path --------------------------------------------------------

func TestEnrollIssuesAUsableIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, false, nil, map[string]string{"site": "wh1"})
	deviceID := mustUUIDv7(t)

	result, err := f.enroll(t, tok, "C02XY1234", deviceID)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	if result.DeviceID != deviceID {
		t.Errorf("device id = %s, want %s", result.DeviceID, deviceID)
	}
	// The token decides the tenant, not the agent.
	if result.OrgID != f.acme.ID {
		t.Errorf("org = %s, want the token's organization %s", result.OrgID, f.acme.ID)
	}
	if result.Approved {
		t.Error("without auto-approve a device must land pending and receive no commands")
	}
	if len(result.CertificatePEM) == 0 || len(result.CABundlePEM) == 0 {
		t.Fatal("certificate and CA bundle must be returned")
	}
	if result.DevicePlaneEndpoint != "edgepilot.test:9443" {
		t.Errorf("endpoint = %q", result.DevicePlaneEndpoint)
	}

	// The certificate must carry the identity we allocated, and verify.
	cert := parseCert(t, result.CertificatePEM)
	identity, err := f.ca.IdentityFromChain([]*x509.Certificate{cert})
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.DeviceID != deviceID || identity.OrgID != f.acme.ID {
		t.Errorf("certificate identity = %s/%s", identity.OrgID, identity.DeviceID)
	}

	// The device row exists, carries the token's labels, and has the
	// certificate recorded.
	scoped := domain.WithOrg(t.Context(), f.acme.ID)
	device, err := f.store.Devices().Get(scoped, deviceID)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if device.Labels["site"] != "wh1" {
		t.Errorf("labels from the token were not stamped: %v", device.Labels)
	}
	current, err := f.store.DeviceCertificates().Current(scoped, deviceID)
	if err != nil {
		t.Fatalf("current certificate: %v", err)
	}
	if string(current.FingerprintSHA256) != string(identity.FingerprintSHA256) {
		t.Error("the stored certificate does not match the one returned")
	}
	if current.IssuedVia != "enrollment" {
		t.Errorf("issued_via = %q", current.IssuedVia)
	}
}

func TestAutoApproveToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, true, nil, nil)
	result, err := f.enroll(t, tok, "AUTO-1", mustUUIDv7(t))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !result.Approved {
		t.Error("an auto-approve token should produce an approved device")
	}
}

// --- token containment -----------------------------------------------------

// A token holder can only enrol into the token's organization. This is the
// containment boundary: a leaked token costs one unwanted device in someone's
// approval queue, not access to another tenant.
func TestTokenDecidesTheTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeToken := f.mintToken(t, f.acme, false, nil, nil)
	deviceID := mustUUIDv7(t)

	result, err := f.enroll(t, acmeToken, "SERIAL-1", deviceID)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if result.OrgID != f.acme.ID {
		t.Fatalf("enrolled into %s, want %s", result.OrgID, f.acme.ID)
	}

	// And the device is invisible to the other tenant.
	if _, err := f.store.Devices().Get(domain.WithOrg(t.Context(), f.globex.ID), deviceID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the enrolled device is visible to another organization: %v", err)
	}
}

// Every token failure mode must be indistinguishable, so a probe cannot
// enumerate valid tokens.
func TestBadTokensAreUniformlyRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	expired := mintExpiredToken(t, f)
	revoked := mintRevokedToken(t, f)
	exhausted := f.mintToken(t, f.acme, false, ptr(1), nil)
	if _, err := f.enroll(t, exhausted, "USED-1", mustUUIDv7(t)); err != nil {
		t.Fatalf("first use should succeed: %v", err)
	}

	var codes []string
	for name, tok := range map[string]string{
		"unknown":   "ep_enroll_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"empty":     "",
		"expired":   expired,
		"revoked":   revoked,
		"exhausted": exhausted,
	} {
		_, err := f.enroll(t, tok, "SERIAL-"+name, mustUUIDv7(t))
		if err == nil {
			t.Fatalf("%s token should have been refused", name)
		}
		codes = append(codes, domain.CodeOf(err))
	}
	for i := range codes {
		if codes[i] != codes[0] {
			t.Errorf("token failures are distinguishable: %v", codes)
			break
		}
	}
	if codes[0] != "invalid_enrollment_token" {
		t.Errorf("code = %q", codes[0])
	}
}

// A single-use token under concurrent enrollment must admit exactly one device.
func TestSingleUseTokenAdmitsOneDevice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, false, ptr(1), nil)

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
	)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Enroll(t.Context(), enrollment.Request{
				Token:        tok,
				DeviceID:     mustUUIDv7(t),
				CSRDer:       newCSR(t, pkix.Name{}),
				SerialNumber: "RACE-" + string(rune('A'+i)),
				Hostname:     "race",
			})
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("%d of %d concurrent enrollments succeeded against a single-use token, want 1",
			successes, racers)
	}
}

// A malformed CSR must not consume one of a single-use token's uses.
func TestMalformedCSRDoesNotConsumeTheToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, false, ptr(1), nil)

	_, err := f.svc.Enroll(t.Context(), enrollment.Request{
		Token:        tok,
		DeviceID:     mustUUIDv7(t),
		CSRDer:       []byte("not a certificate request"),
		SerialNumber: "BAD-CSR",
		Hostname:     "bad",
	})
	if err == nil {
		t.Fatal("a malformed CSR must be rejected")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected a validation error, got: %v", err)
	}

	// The token must still work.
	if _, err := f.enroll(t, tok, "GOOD-1", mustUUIDv7(t)); err != nil {
		t.Errorf("the token should not have been consumed by the failed attempt: %v", err)
	}
}

// --- CSR identity ----------------------------------------------------------

// The CSR is attacker-controlled. Identity on the issued certificate must come
// from the token and the device record, never from the request.
func TestCSRCannotChooseItsIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// A device already enrolled in the other tenant, which the attacker wants
	// to impersonate.
	victimToken := f.mintToken(t, f.globex, true, nil, nil)
	victimID := mustUUIDv7(t)
	if _, err := f.enroll(t, victimToken, "VICTIM-1", victimID); err != nil {
		t.Fatalf("victim enroll: %v", err)
	}

	// The attacker holds only an Acme token, but asks to be the Globex device.
	attackerToken := f.mintToken(t, f.acme, false, nil, nil)
	attackerID := mustUUIDv7(t)

	result, err := f.svc.Enroll(t.Context(), enrollment.Request{
		Token:    attackerToken,
		DeviceID: attackerID,
		CSRDer: newCSR(t, pkix.Name{
			CommonName:   victimID.String(),
			Organization: []string{f.globex.ID.String()},
		}),
		SerialNumber: "ATTACKER-1",
		Hostname:     "attacker",
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	cert := parseCert(t, result.CertificatePEM)
	identity, err := f.ca.IdentityFromChain([]*x509.Certificate{cert})
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.DeviceID == victimID {
		t.Fatal("the CSR's requested device identity was honoured")
	}
	if identity.OrgID != f.acme.ID {
		t.Fatalf("certificate names organization %s, want the token's %s", identity.OrgID, f.acme.ID)
	}
}

// --- re-enrollment ---------------------------------------------------------

// Re-enrolling supersedes the previous identity. Leaving the old certificate
// live would let a re-imaged Mac — or a key copied off one before a rebuild —
// keep connecting.
func TestReEnrollmentRevokesThePreviousCertificate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, true, nil, nil)
	deviceID := mustUUIDv7(t)

	first, err := f.enroll(t, tok, "REENROLL-1", deviceID)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	firstFingerprint := fingerprintOf(t, f, first)

	second, err := f.enroll(t, tok, "REENROLL-1", mustUUIDv7(t))
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if second.DeviceID != deviceID {
		t.Errorf("re-enrollment should reclaim the existing device row, got %s", second.DeviceID)
	}

	revoked, err := f.store.DeviceCertificates().ListRevokedFingerprints(t.Context())
	if err != nil {
		t.Fatalf("list revoked: %v", err)
	}
	found := false
	for _, fp := range revoked {
		if string(fp) == string(firstFingerprint) {
			found = true
		}
	}
	if !found {
		t.Error("the superseded certificate should have been revoked")
	}

	// And the new one is current.
	current, err := f.store.DeviceCertificates().Current(domain.WithOrg(t.Context(), f.acme.ID), deviceID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if string(current.FingerprintSHA256) == string(firstFingerprint) {
		t.Error("the old certificate is still current")
	}
}

// --- renewal ---------------------------------------------------------------

func TestRenewIssuesWithoutAToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, true, nil, nil)
	deviceID := mustUUIDv7(t)
	first, err := f.enroll(t, tok, "RENEW-1", deviceID)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	firstFingerprint := fingerprintOf(t, f, first)

	// Identity comes from the authenticated connection, not from any token.
	renewed, err := f.svc.Renew(t.Context(), enrollment.RenewRequest{
		DeviceID: deviceID,
		OrgID:    f.acme.ID,
		CSRDer:   newCSR(t, pkix.Name{}),
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	cert := parseCert(t, renewed.CertificatePEM)
	identity, err := f.ca.IdentityFromChain([]*x509.Certificate{cert})
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.DeviceID != deviceID || identity.OrgID != f.acme.ID {
		t.Error("the renewed certificate carries the wrong identity")
	}

	// The previous certificate is deliberately NOT revoked: the agent has to
	// receive the new one and swap it in, and killing the old one first would
	// disconnect the device mid-renewal with no way to retry.
	revoked, err := f.store.DeviceCertificates().ListRevokedFingerprints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, fp := range revoked {
		if string(fp) == string(firstFingerprint) {
			t.Error("renewal must not revoke the certificate the agent is still using")
		}
	}
}

func TestRenewRefusesRevokedDevices(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, true, nil, nil)
	deviceID := mustUUIDv7(t)
	if _, err := f.enroll(t, tok, "REVOKED-1", deviceID); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	scoped := domain.WithOrg(t.Context(), f.acme.ID)
	if _, err := f.store.Devices().SetEnrollmentState(scoped, deviceID, domain.EnrollmentRevoked); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Renewing would undo the revocation.
	if _, err := f.svc.Renew(t.Context(), enrollment.RenewRequest{
		DeviceID: deviceID, OrgID: f.acme.ID, CSRDer: newCSR(t, pkix.Name{}),
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a revoked device must not be able to renew, got: %v", err)
	}
}

func TestRenewRequiresAnAuthenticatedIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for name, req := range map[string]enrollment.RenewRequest{
		"no device": {OrgID: f.acme.ID, CSRDer: newCSR(t, pkix.Name{})},
		"no org":    {DeviceID: mustUUIDv7(t), CSRDer: newCSR(t, pkix.Name{})},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.Renew(t.Context(), req); err == nil {
				t.Error("renewal without an authenticated identity must be refused")
			}
		})
	}
}

// --- approval --------------------------------------------------------------

func TestApproveRequiresPermission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, false, nil, nil)
	deviceID := mustUUIDv7(t)
	if _, err := f.enroll(t, tok, "APPROVE-1", deviceID); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	scoped := domain.WithOrg(t.Context(), f.acme.ID)

	// An operator does not hold device:approve — enrolling a rogue device
	// should not be self-service.
	operator := principalWith(f.acme.ID, domain.PermDeviceRead, domain.PermCommandExecute)
	if _, err := f.svc.Approve(domain.WithPrincipal(scoped, operator), deviceID, true); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an operator must not be able to approve devices, got: %v", err)
	}

	// Unauthenticated is refused too.
	if _, err := f.svc.Approve(scoped, deviceID, true); err == nil {
		t.Error("an unauthenticated caller must not approve devices")
	}

	admin := principalWith(f.acme.ID, domain.PermDeviceApprove)
	device, err := f.svc.Approve(domain.WithPrincipal(scoped, admin), deviceID, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !device.Approved() {
		t.Error("the device should now be approved")
	}
}

// Rejecting a device must revoke its certificate — otherwise it reconnects and
// keeps reporting, which is exactly what rejection was meant to stop.
func TestRejectRevokesTheCertificate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, false, nil, nil)
	deviceID := mustUUIDv7(t)
	result, err := f.enroll(t, tok, "REJECT-1", deviceID)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	fingerprint := fingerprintOf(t, f, result)

	scoped := domain.WithPrincipal(
		domain.WithOrg(t.Context(), f.acme.ID),
		principalWith(f.acme.ID, domain.PermDeviceApprove))

	device, err := f.svc.Approve(scoped, deviceID, false)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if device.EnrollmentState != domain.EnrollmentRejected {
		t.Errorf("state = %q, want rejected", device.EnrollmentState)
	}

	revoked, err := f.store.DeviceCertificates().ListRevokedFingerprints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, fp := range revoked {
		if string(fp) == string(fingerprint) {
			found = true
		}
	}
	if !found {
		t.Error("rejecting a device must revoke its certificate")
	}
}

func TestApproveRejectsAlreadyDecidedDevices(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tok := f.mintToken(t, f.acme, true, nil, nil) // already approved
	deviceID := mustUUIDv7(t)
	if _, err := f.enroll(t, tok, "DECIDED-1", deviceID); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	scoped := domain.WithPrincipal(
		domain.WithOrg(t.Context(), f.acme.ID),
		principalWith(f.acme.ID, domain.PermDeviceApprove))

	if _, err := f.svc.Approve(scoped, deviceID, true); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("approving an already-approved device should conflict, got: %v", err)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()

	h, err := ca.Generate(ca.GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ca.New(ca.Options{SigningCertPEM: h.ChainPEM, SigningKeyPEM: h.IntermediateKeyPEM})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := enrollment.New(enrollment.Options{CA: authority}); err == nil {
		t.Error("a store is required")
	}
	if _, err := enrollment.New(enrollment.Options{Store: &postgres.Store{}}); err == nil {
		t.Error("a CA is required")
	}
}

// --- helpers ---------------------------------------------------------------

func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("no PEM block in the returned certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func fingerprintOf(t *testing.T, f *fixture, r *enrollment.Result) []byte {
	t.Helper()
	cert := parseCert(t, r.CertificatePEM)
	identity, err := f.ca.IdentityFromChain([]*x509.Certificate{cert})
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return identity.FingerprintSHA256
}

func mintExpiredToken(t *testing.T, f *fixture) string {
	t.Helper()
	plaintext, hash, prefix, err := token.NewEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	// Created with a future expiry — a past one is rejected at creation — then
	// aged out by updating the row directly.
	if _, err := f.store.EnrollmentTokens().Create(
		domain.WithOrg(t.Context(), f.acme.ID),
		domain.CreateEnrollmentTokenInput{
			OrgID: f.acme.ID, ExpiresAt: time.Now().Add(time.Hour),
		}, hash, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Pool().Exec(t.Context(),
		`UPDATE enrollment_tokens SET expires_at = now() - interval '1 hour' WHERE token_hash = $1`,
		hash); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func mintRevokedToken(t *testing.T, f *fixture) string {
	t.Helper()
	plaintext, hash, prefix, err := token.NewEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	scoped := domain.WithOrg(t.Context(), f.acme.ID)
	tok, err := f.store.EnrollmentTokens().Create(scoped, domain.CreateEnrollmentTokenInput{
		OrgID: f.acme.ID, ExpiresAt: time.Now().Add(time.Hour),
	}, hash, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.EnrollmentTokens().Revoke(scoped, tok.ID); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func principalWith(orgID uuid.UUID, perms ...string) *domain.Principal {
	set := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		set[p] = struct{}{}
	}
	return &domain.Principal{
		UserID:      uuid.New(),
		OrgID:       orgID,
		SessionID:   uuid.New(),
		Permissions: set,
	}
}

func mustUUIDv7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ptr[T any](v T) *T { return &v }
