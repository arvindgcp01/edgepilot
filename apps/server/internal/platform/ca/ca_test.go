package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestCA(t *testing.T) *Authority {
	t.Helper()
	h, err := Generate(GenerateOptions{Organization: "EdgePilot Test"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	a, err := New(Options{SigningCertPEM: h.ChainPEM, SigningKeyPEM: h.IntermediateKeyPEM})
	if err != nil {
		t.Fatalf("new authority: %v", err)
	}
	return a
}

// makeCSR builds a certificate request, optionally with attacker-chosen
// identity fields, signed by a freshly generated key.
func makeCSR(t *testing.T, template *x509.CertificateRequest) ([]byte, crypto.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if template == nil {
		template = &x509.CertificateRequest{}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return der, key
}

// --- the attack this package exists to prevent -----------------------------

// A CSR is entirely attacker-controlled. Every identity field on the issued
// certificate must come from server-side state, never from the request.
func TestIdentityIsNeverTakenFromTheCSR(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	realDevice := uuid.New()
	realOrg := uuid.New()

	victimDevice := uuid.New()
	victimOrg := uuid.New()

	// The requester asks to be a different device, in a different tenant, with
	// a CA certificate and server-auth rights.
	csrDER, _ := makeCSR(t, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   victimDevice.String(),
			Organization: []string{victimOrg.String()},
		},
		URIs: []*url.URL{{
			Scheme: "spiffe",
			Host:   TrustDomain,
			Path:   "/" + victimOrg.String() + "/" + victimDevice.String(),
		}},
		DNSNames:       []string{"edgepilot.visionify.ai"},
		EmailAddresses: []string{"admin@visionify.ai"},
	})

	issued, err := a.Sign(csrDER, realDevice, realOrg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	cert := issued.Certificate

	// Identity comes from the arguments, not the request.
	if cert.Subject.CommonName != realDevice.String() {
		t.Errorf("CN = %q, want the device id we allocated", cert.Subject.CommonName)
	}
	if cert.Subject.CommonName == victimDevice.String() {
		t.Error("the CSR's requested identity was honoured")
	}

	device, org, err := parseSPIFFE(cert)
	if err != nil {
		t.Fatalf("parse spiffe: %v", err)
	}
	if device != realDevice || org != realOrg {
		t.Errorf("SAN identity = %s/%s, want %s/%s", org, device, realOrg, realDevice)
	}

	// Nothing else the requester asked for survived.
	if len(cert.DNSNames) != 0 {
		t.Errorf("DNS names from the CSR were copied: %v", cert.DNSNames)
	}
	if len(cert.EmailAddresses) != 0 {
		t.Errorf("email addresses from the CSR were copied: %v", cert.EmailAddresses)
	}
	if len(cert.URIs) != 1 {
		t.Errorf("expected exactly the SPIFFE URI we set, got %v", cert.URIs)
	}
}

// A CSR is self-signed. Verifying that signature is the only evidence the
// requester actually holds the private key for the public key inside.
func TestCSRSignatureIsVerified(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	csrDER, _ := makeCSR(t, nil)

	// Corrupt the signature. The structure stays valid, so only a real
	// signature check catches this.
	tampered := make([]byte, len(csrDER))
	copy(tampered, csrDER)
	tampered[len(tampered)-1] ^= 0xff
	tampered[len(tampered)-2] ^= 0xff

	if _, err := a.Sign(tampered, uuid.New(), uuid.New()); err == nil {
		t.Fatal("a CSR with a broken signature must not be signed")
	}
}

// Issued certificates must not be usable as a CA, or one compromised device
// could mint certificates for every other device in the fleet.
func TestIssuedCertificateIsNotACA(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	// The requester explicitly asks for CA rights via a requested extension.
	csrDER, _ := makeCSR(t, &x509.CertificateRequest{
		ExtraExtensions: []pkix.Extension{{
			Id:       []int{2, 5, 29, 19}, // basicConstraints
			Critical: true,
			Value:    []byte{0x30, 0x03, 0x01, 0x01, 0xff},
		}},
	})

	issued, err := a.Sign(csrDER, uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if issued.Certificate.IsCA {
		t.Error("the issued certificate is a CA; a device could mint certificates for the whole fleet")
	}
	if !issued.Certificate.BasicConstraintsValid {
		t.Error("basicConstraints must be present and marked valid")
	}
}

// Client authentication only. A device certificate valid for server auth could
// be used to impersonate the control plane to other agents.
func TestIssuedCertificateIsClientAuthOnly(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	csrDER, _ := makeCSR(t, nil)
	issued, err := a.Sign(csrDER, uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if len(issued.Certificate.ExtKeyUsage) != 1 ||
		issued.Certificate.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("extended key usage = %v, want client auth only", issued.Certificate.ExtKeyUsage)
	}
	for _, u := range issued.Certificate.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			t.Error("a device certificate must not be valid for server authentication")
		}
	}
}

func TestWeakKeysAreRejected(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	t.Run("P-224", func(t *testing.T) {
		t.Parallel()
		key, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
		if err != nil {
			t.Skipf("P-224 unavailable: %v", err)
		}
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if err != nil {
			t.Skipf("P-224 CSR unsupported: %v", err)
		}
		if _, err := a.Sign(der, uuid.New(), uuid.New()); !errors.Is(err, ErrWeakKey) {
			t.Errorf("P-224 should be rejected as weak, got: %v", err)
		}
	})

	t.Run("RSA-2048", func(t *testing.T) {
		t.Parallel()
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Sign(der, uuid.New(), uuid.New()); !errors.Is(err, ErrWeakKey) {
			t.Errorf("RSA-2048 should be below the floor, got: %v", err)
		}
	})

	t.Run("accepted key types", func(t *testing.T) {
		t.Parallel()
		for name, key := range map[string]crypto.Signer{
			"P-256":   mustECDSA(t, elliptic.P256()),
			"P-384":   mustECDSA(t, elliptic.P384()),
			"Ed25519": mustEd25519(t),
		} {
			der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
			if err != nil {
				t.Fatalf("%s: create csr: %v", name, err)
			}
			if _, err := a.Sign(der, uuid.New(), uuid.New()); err != nil {
				t.Errorf("%s should be accepted: %v", name, err)
			}
		}
	})
}

func TestMalformedCSRsAreRejected(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	for name, der := range map[string][]byte{
		"empty":     nil,
		"garbage":   []byte("this is not a certificate request"),
		"truncated": func() []byte { d, _ := makeCSR(t, nil); return d[:len(d)/2] }(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := a.Sign(der, uuid.New(), uuid.New()); err == nil {
				t.Error("a malformed CSR must be rejected")
			}
		})
	}
}

func TestSignRequiresIdentity(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)
	csrDER, _ := makeCSR(t, nil)

	if _, err := a.Sign(csrDER, uuid.Nil, uuid.New()); err == nil {
		t.Error("a nil device id must be rejected")
	}
	if _, err := a.Sign(csrDER, uuid.New(), uuid.Nil); err == nil {
		t.Error("a nil organization id must be rejected")
	}
}

// --- serials ---------------------------------------------------------------

// Sequential serials disclose how many certificates have been issued — and so
// roughly how many devices exist — to anyone holding one, and make the next
// serial predictable.
func TestSerialsAreRandomAndUnique(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	seen := map[string]struct{}{}
	for range 50 {
		csrDER, _ := makeCSR(t, nil)
		issued, err := a.Sign(csrDER, uuid.New(), uuid.New())
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		if _, dup := seen[issued.SerialDecimal]; dup {
			t.Fatal("a serial repeated")
		}
		seen[issued.SerialDecimal] = struct{}{}

		if issued.Certificate.SerialNumber.Sign() <= 0 {
			t.Error("RFC 5280 requires a positive serial")
		}
		// 128 bits of entropy: a sequential counter would be far smaller.
		if issued.Certificate.SerialNumber.BitLen() < 64 {
			t.Errorf("serial has only %d bits; this looks sequential",
				issued.Certificate.SerialNumber.BitLen())
		}
	}
}

// --- chain verification ----------------------------------------------------

// The issued certificate must actually verify against the CA the agent pins,
// or nothing will connect.
func TestIssuedCertificateVerifiesAgainstTheChain(t *testing.T) {
	t.Parallel()

	h, err := Generate(GenerateOptions{Organization: "EdgePilot Test"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{SigningCertPEM: h.ChainPEM, SigningKeyPEM: h.IntermediateKeyPEM})
	if err != nil {
		t.Fatal(err)
	}

	csrDER, _ := makeCSR(t, nil)
	issued, err := a.Sign(csrDER, uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	roots, err := parseCertChain(h.RootCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	rootPool := x509.NewCertPool()
	rootPool.AddCert(roots[0])

	intermediates, err := parseCertChain(h.IntermediateCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	interPool := x509.NewCertPool()
	interPool.AddCert(intermediates[0])

	if _, err := issued.Certificate.Verify(x509.VerifyOptions{
		Roots:         rootPool,
		Intermediates: interPool,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("the issued certificate does not chain to the root: %v", err)
	}
}

// A certificate from a different CA must not be accepted as an identity, even
// if it is well-formed and carries a valid-looking SPIFFE SAN.
func TestIdentityRejectsForeignCertificates(t *testing.T) {
	t.Parallel()

	ours := newTestCA(t)
	theirs := newTestCA(t)

	deviceID, orgID := uuid.New(), uuid.New()
	csrDER, _ := makeCSR(t, nil)
	foreign, err := theirs.Sign(csrDER, deviceID, orgID)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := ours.IdentityFromChain([]*x509.Certificate{foreign.Certificate}); !errors.Is(err, ErrWrongIssuer) {
		t.Errorf("a certificate from another CA must be refused, got: %v", err)
	}
}

func TestIdentityFromChain(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	deviceID, orgID := uuid.New(), uuid.New()
	csrDER, _ := makeCSR(t, nil)
	issued, err := a.Sign(csrDER, deviceID, orgID)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	identity, err := a.IdentityFromChain([]*x509.Certificate{issued.Certificate})
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.DeviceID != deviceID || identity.OrgID != orgID {
		t.Errorf("identity = %s/%s, want %s/%s", identity.OrgID, identity.DeviceID, orgID, deviceID)
	}
	if len(identity.FingerprintSHA256) != 32 {
		t.Errorf("fingerprint is %d bytes, want 32", len(identity.FingerprintSHA256))
	}
	if !identity.NotAfter.Equal(issued.NotAfter.Truncate(time.Second)) &&
		identity.NotAfter.Sub(issued.NotAfter).Abs() > time.Second {
		t.Errorf("expiry mismatch: %v vs %v", identity.NotAfter, issued.NotAfter)
	}

	if _, err := a.IdentityFromChain(nil); err == nil {
		t.Error("an empty chain must be refused")
	}
}

// The identity comes from the SAN, not the Common Name. A certificate with a
// plausible CN but no SPIFFE SAN carries no identity.
func TestIdentityIgnoresTheCommonName(t *testing.T) {
	t.Parallel()

	h, err := Generate(GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{SigningCertPEM: h.ChainPEM, SigningKeyPEM: h.IntermediateKeyPEM})
	if err != nil {
		t.Fatal(err)
	}

	// Hand-craft a leaf signed by the real intermediate but with only a CN.
	interKey, err := parsePrivateKey(h.IntermediateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	interCerts, err := parseCertChain(h.IntermediateCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: uuid.New().String()},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}, interCerts[0], &leafKey.PublicKey, interKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.IdentityFromChain([]*x509.Certificate{leaf}); !errors.Is(err, ErrNoDeviceIdentity) {
		t.Errorf("a certificate with only a CN carries no identity, got: %v", err)
	}
}

// --- generation ------------------------------------------------------------

func TestGenerateProducesAConstrainedHierarchy(t *testing.T) {
	t.Parallel()

	h, err := Generate(GenerateOptions{Organization: "EdgePilot Test"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	roots, err := parseCertChain(h.RootCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	inters, err := parseCertChain(h.IntermediateCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	root, inter := roots[0], inters[0]

	if !root.IsCA || !inter.IsCA {
		t.Error("both CA certificates must be marked as CAs")
	}
	// The root signs intermediates; an intermediate signs leaves and nothing
	// else. A compromised intermediate cannot mint another CA below itself.
	if root.MaxPathLen != 1 {
		t.Errorf("root MaxPathLen = %d, want 1", root.MaxPathLen)
	}
	if inter.MaxPathLen != 0 || !inter.MaxPathLenZero {
		t.Errorf("intermediate must have MaxPathLen 0, got %d (zero=%v)",
			inter.MaxPathLen, inter.MaxPathLenZero)
	}
	// Even holding the intermediate key, an attacker cannot issue a certificate
	// valid for server authentication and impersonate the control plane.
	for _, u := range inter.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			t.Error("the intermediate must not be permitted to issue server certificates")
		}
	}
	if inter.NotAfter.After(root.NotAfter) {
		t.Error("the intermediate must not outlive the root")
	}

	// The intermediate must actually chain to the root.
	rootPool := x509.NewCertPool()
	rootPool.AddCert(root)
	if _, err := inter.Verify(x509.VerifyOptions{Roots: rootPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("the intermediate does not chain to the root: %v", err)
	}
}

func TestGenerateRejectsAnIntermediateOutlivingTheRoot(t *testing.T) {
	t.Parallel()

	_, err := Generate(GenerateOptions{
		RootTTL:         time.Hour,
		IntermediateTTL: 2 * time.Hour,
	})
	if err == nil {
		t.Error("an intermediate that outlives its root must be rejected")
	}
}

func TestNewRejectsBadMaterial(t *testing.T) {
	t.Parallel()

	h, err := Generate(GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := Generate(GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A mismatched key and certificate would produce certificates nothing can
	// verify. Catching it at startup beats discovering it when the first device
	// fails to connect.
	if _, err := New(Options{
		SigningCertPEM: h.ChainPEM,
		SigningKeyPEM:  other.IntermediateKeyPEM,
	}); err == nil {
		t.Error("a key that does not match the certificate must be rejected")
	}

	if _, err := New(Options{SigningCertPEM: nil, SigningKeyPEM: h.IntermediateKeyPEM}); err == nil {
		t.Error("missing certificate material must be rejected")
	}
	if _, err := New(Options{SigningCertPEM: h.ChainPEM, SigningKeyPEM: nil}); err == nil {
		t.Error("missing key material must be rejected")
	}
	if _, err := New(Options{
		SigningCertPEM: []byte("-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydA\n-----END CERTIFICATE-----\n"),
		SigningKeyPEM:  h.IntermediateKeyPEM,
	}); err == nil {
		t.Error("an unparseable certificate must be rejected")
	}
}

// A leaf certificate must not be usable as the signing certificate — that would
// silently produce a CA that cannot issue anything valid.
func TestNewRejectsANonCASigningCertificate(t *testing.T) {
	t.Parallel()

	a := newTestCA(t)
	csrDER, _ := makeCSR(t, nil)
	issued, err := a.Sign(csrDER, uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := New(Options{
		SigningCertPEM: issued.CertificatePEM,
		SigningKeyPEM:  []byte("irrelevant"),
	}); err == nil {
		t.Error("a leaf certificate must not be accepted as a signing certificate")
	}
}

func TestChainPEMCarriesTheWholePath(t *testing.T) {
	t.Parallel()
	a := newTestCA(t)

	chain, err := parseCertChain(a.ChainPEM())
	if err != nil {
		t.Fatalf("parse chain: %v", err)
	}
	// The agent needs the intermediate and the root to verify and to pin.
	if len(chain) != 2 {
		t.Errorf("chain has %d certificates, want 2 (intermediate then root)", len(chain))
	}
	if !chain[0].IsCA || !chain[1].IsCA {
		t.Error("both chain entries should be CA certificates")
	}
}

func TestDeviceCertificateLifetime(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	h, err := Generate(GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SigningCertPEM: h.ChainPEM,
		SigningKeyPEM:  h.IntermediateKeyPEM,
		DeviceTTL:      90 * 24 * time.Hour,
		Now:            func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatal(err)
	}

	csrDER, _ := makeCSR(t, nil)
	issued, err := a.Sign(csrDER, uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Backdated slightly so a device with a slow clock does not reject its own
	// certificate as not-yet-valid — a state it cannot recover from without the
	// connection the certificate provides.
	if !issued.NotBefore.Before(fixed) {
		t.Errorf("notBefore = %v, should be backdated from %v", issued.NotBefore, fixed)
	}
	if got := issued.NotAfter.Sub(fixed); got != 90*24*time.Hour {
		t.Errorf("lifetime = %v, want 90 days", got)
	}
}

func mustECDSA(t *testing.T, curve elliptic.Curve) crypto.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustEd25519(t *testing.T) crypto.Signer {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
