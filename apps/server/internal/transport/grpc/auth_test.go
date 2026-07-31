package grpc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
	epgrpc "github.com/visionify/edgepilot/apps/server/internal/transport/grpc"
)

// These tests run real TLS handshakes over a real socket rather than asserting
// on configuration fields. The property being checked — that a revoked or
// unknown device cannot establish a connection — is a property of the handshake,
// and a test that only inspected tls.Config would pass while the handshake
// accepted everything.

type harness struct {
	authority *ca.Authority
	chainPEM  []byte
	serverTLS *tls.Config
	revoked   *fakeRevocation
}

type fakeRevocation struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

func newFakeRevocation() *fakeRevocation {
	return &fakeRevocation{set: map[string]struct{}{}}
}

func (f *fakeRevocation) IsRevoked(fingerprint []byte) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.set[string(fingerprint)]
	return ok
}

func (f *fakeRevocation) revoke(fingerprint []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set[string(fingerprint)] = struct{}{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	hierarchy, err := ca.Generate(ca.GenerateOptions{Organization: "EdgePilot Test"})
	if err != nil {
		t.Fatalf("generate ca: %v", err)
	}
	authority, err := ca.New(ca.Options{
		SigningCertPEM: hierarchy.ChainPEM,
		SigningKeyPEM:  hierarchy.IntermediateKeyPEM,
	})
	if err != nil {
		t.Fatalf("new authority: %v", err)
	}

	serverCert := serverCertificate(t, hierarchy)
	revoked := newFakeRevocation()

	return &harness{
		authority: authority,
		chainPEM:  hierarchy.ChainPEM,
		serverTLS: epgrpc.TLSConfig(authority, serverCert, revoked, slog.New(slog.DiscardHandler)),
		revoked:   revoked,
	}
}

// serverCertificate mints the listener's own certificate, signed by the same
// root so an agent can verify it with the bundle it was given at enrollment.
func serverCertificate(t *testing.T, h *ca.Hierarchy) tls.Certificate {
	t.Helper()

	rootBlock, _ := pem.Decode(h.RootCertPEM)
	rootCert, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(h.RootKeyPEM)
	rootKeyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rootKey := rootKeyAny.(*ecdsa.PrivateKey)

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "edgepilot-device-plane"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}, rootCert, &serverKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: serverKey}
}

// enrolDevice issues a device certificate and returns a client TLS config using
// it, exactly as an agent would.
func (h *harness) enrolDevice(t *testing.T, deviceID, orgID uuid.UUID) (*tls.Config, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := h.authority.Sign(csr, deviceID, orgID)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(h.chainPEM) {
		t.Fatal("could not build the client root pool")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{issued.Certificate.Raw},
			PrivateKey:  key,
		}},
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}, issued.FingerprintSHA256
}

// handshake attempts a TLS connection and returns the server's view of it.
func (h *harness) handshake(t *testing.T, clientTLS *tls.Config) (tls.ConnectionState, error) {
	t.Helper()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", h.serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	type result struct {
		state tls.ConnectionState
		err   error
	}
	serverDone := make(chan result, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- result{err: acceptErr}
			return
		}
		defer func() { _ = conn.Close() }()

		tlsConn := conn.(*tls.Conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if hsErr := tlsConn.HandshakeContext(ctx); hsErr != nil {
			serverDone <- result{err: hsErr}
			return
		}
		serverDone <- result{state: tlsConn.ConnectionState()}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dialer := &tls.Dialer{Config: clientTLS}
	clientConn, _ := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if clientConn != nil {
		// TLS 1.3 completes the client handshake before the server has
		// processed the client certificate, so a rejection surfaces on the
		// first read rather than at dial. This read forces the server to reach
		// its decision; the client's own error is not what these tests assert
		// on, because the server's verdict is the security property.
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = clientConn.Read(make([]byte, 1))
		_ = clientConn.Close()
	}

	select {
	case r := <-serverDone:
		if r.err != nil {
			return tls.ConnectionState{}, r.err
		}
		return r.state, nil
	case <-time.After(10 * time.Second):
		t.Fatal("handshake did not complete")
		return tls.ConnectionState{}, nil
	}
}

// --- the properties that matter --------------------------------------------

func TestEnrolledDeviceCanConnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	deviceID, orgID := uuid.New(), uuid.New()
	clientTLS, _ := h.enrolDevice(t, deviceID, orgID)

	state, err := h.handshake(t, clientTLS)
	if err != nil {
		t.Fatalf("an enrolled device should connect: %v", err)
	}

	// And the server can read its identity straight off the connection, with no
	// application-level token involved.
	identity, err := h.authority.IdentityFromChain(state.VerifiedChains[0])
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.DeviceID != deviceID || identity.OrgID != orgID {
		t.Errorf("identity = %s/%s, want %s/%s",
			identity.OrgID, identity.DeviceID, orgID, deviceID)
	}
}

// Revocation is enforced during the handshake, so a revoked device never
// reaches a stream — rather than being refused by whichever handler remembers
// to check.
func TestRevokedDeviceCannotConnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	clientTLS, fingerprint := h.enrolDevice(t, uuid.New(), uuid.New())

	// Connects normally before revocation.
	if _, err := h.handshake(t, clientTLS); err != nil {
		t.Fatalf("should connect before revocation: %v", err)
	}

	h.revoked.revoke(fingerprint)

	if _, err := h.handshake(t, clientTLS); err == nil {
		t.Fatal("a revoked device completed the handshake")
	}
}

// A certificate from another CA must not connect, however well-formed.
func TestForeignCertificateCannotConnect(t *testing.T) {
	t.Parallel()

	ours := newHarness(t)
	theirs := newHarness(t)

	// A perfectly valid device certificate — from the wrong authority.
	foreignTLS, _ := theirs.enrolDevice(t, uuid.New(), uuid.New())
	// Point it at our server, trusting our root so the client side succeeds and
	// only the server's client-certificate check can reject it.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ours.chainPEM) {
		t.Fatal("root pool")
	}
	foreignTLS.RootCAs = roots

	if _, err := ours.handshake(t, foreignTLS); err == nil {
		t.Fatal("a certificate from another CA completed the handshake")
	}
}

// Not VerifyClientCertIfGiven: an agent with no certificate must fail the
// handshake, not arrive as an anonymous peer something downstream has to
// remember to reject.
func TestConnectionWithoutAClientCertificateIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(h.chainPEM) {
		t.Fatal("root pool")
	}

	if _, err := h.handshake(t, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	}); err == nil {
		t.Fatal("a connection with no client certificate was accepted")
	}
}

// Every agent is software we ship, so there is no legacy client to accommodate.
func TestTLS13IsRequired(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	clientTLS, _ := h.enrolDevice(t, uuid.New(), uuid.New())
	clientTLS.MinVersion = tls.VersionTLS12
	clientTLS.MaxVersion = tls.VersionTLS12

	if _, err := h.handshake(t, clientTLS); err == nil {
		t.Fatal("a TLS 1.2 connection was accepted")
	}

	if h.serverTLS.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3", h.serverTLS.MinVersion)
	}
	if h.serverTLS.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", h.serverTLS.ClientAuth)
	}
}

// --- context plumbing ------------------------------------------------------

func TestDeviceIdentityContext(t *testing.T) {
	t.Parallel()

	if _, ok := epgrpc.DeviceIdentityFromContext(context.Background()); ok {
		t.Error("a bare context must not yield an identity")
	}

	// A nil identity must not read as authenticated: a handler that got one
	// would proceed with a zero-valued device.
	//nolint:staticcheck // deliberately storing nil to prove the guard
	if _, ok := epgrpc.DeviceIdentityFromContext(epgrpc.WithDeviceIdentity(context.Background(), nil)); ok {
		t.Error("a nil identity must not read as authenticated")
	}

	want := &ca.Identity{DeviceID: uuid.New(), OrgID: uuid.New()}
	got, ok := epgrpc.DeviceIdentityFromContext(epgrpc.WithDeviceIdentity(context.Background(), want))
	if !ok {
		t.Fatal("an attached identity should be retrievable")
	}
	if got.DeviceID != want.DeviceID || got.OrgID != want.OrgID {
		t.Error("identity did not round-trip")
	}

	device, org := epgrpc.DeviceOrOrg(nil)
	if device != uuid.Nil || org != uuid.Nil {
		t.Error("DeviceOrOrg(nil) should yield nil ids rather than panicking")
	}
}

// A plaintext connection reaching the device plane is a misconfiguration, not an
// attack — but it must not be served.
func TestInterceptorRefusesUnauthenticatedContexts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	interceptor := epgrpc.NewAuthInterceptor(h.authority, slog.New(slog.DiscardHandler))
	handlerRan := false

	err := interceptor.Stream()(nil, fakeServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error {
			handlerRan = true
			return nil
		})
	if err == nil {
		t.Fatal("a context with no peer information must be refused")
	}
	if handlerRan {
		t.Error("the handler ran for an unauthenticated connection")
	}
	if !strings.Contains(err.Error(), "peer") && !strings.Contains(err.Error(), "authenticated") {
		t.Errorf("unexpected error: %v", err)
	}
}

// fakeServerStream is a grpc.ServerStream carrying only a context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeServerStream) Context() context.Context { return f.ctx }
