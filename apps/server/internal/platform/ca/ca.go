// Package ca is EdgePilot's private certificate authority for device identity.
//
// Every Mac authenticates to the device plane with an mTLS client certificate
// issued here. That makes device identity a transport-layer property: there is
// no application-level device token to leak, log, replay, or forget to rotate,
// and revocation is immediate rather than bounded by a token lifetime.
//
// # The rule that matters most
//
// A CSR is entirely attacker-controlled. It is signed by the key it contains,
// which proves possession of that key and *nothing else* — not the subject, not
// the SANs, not the extensions. A CA that copies identity fields out of a CSR
// will issue a certificate for whatever the requester asked to be.
//
// So this package reads exactly two things from a CSR: the public key, and the
// signature proving the requester holds the matching private key. Every
// identity field on the issued certificate comes from server-side state — the
// device id the enrollment service allocated, the organization the redeemed
// token belongs to. Sign() does not even accept a subject parameter.
package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Errors returned by this package.
var (
	ErrBadCSR           = errors.New("certificate request is malformed")
	ErrWeakKey          = errors.New("certificate request uses a key that is too weak")
	ErrCSRSignature     = errors.New("certificate request signature is invalid")
	ErrNotClientCert    = errors.New("certificate is not a device client certificate")
	ErrNoDeviceIdentity = errors.New("certificate carries no device identity")
	ErrWrongIssuer      = errors.New("certificate was not issued by this authority")
)

// SPIFFE trust domain for device identities.
//
// The SAN is spiffe://edgepilot/<org-uuid>/<device-uuid>, so a peer certificate
// answers both "which device" and "which tenant" without a database lookup — the
// gRPC interceptor needs the org to scope the connection before it can query
// anything.
const TrustDomain = "edgepilot"

// Lifetimes.
const (
	// RootTTL is deliberately long: rotating a root means re-enrolling every
	// device in the fleet, so it should outlive the hardware.
	RootTTL = 20 * 365 * 24 * time.Hour

	// IntermediateTTL is shorter. The intermediate is the key that actually
	// lives on a server and can therefore be stolen; the root should be offline.
	IntermediateTTL = 5 * 365 * 24 * time.Hour

	// DeviceTTL is short because renewal is automatic over the authenticated
	// stream. Ninety days bounds how long a certificate that escaped
	// unnoticed — one copied off a decommissioned Mac, say — stays usable.
	DeviceTTL = 90 * 24 * time.Hour
)

// Authority issues device certificates.
type Authority struct {
	// cert is the signing certificate, normally the intermediate.
	cert *x509.Certificate
	// key is its private key.
	key crypto.Signer
	// chain is the full path to the root, served to agents so they can verify
	// the server and pin the root.
	chain []*x509.Certificate
	// pool verifies presented device certificates.
	pool *x509.CertPool

	ttl time.Duration
	now func() time.Time
}

// Options configures an Authority.
type Options struct {
	// SigningCertPEM is the issuing certificate, followed optionally by the
	// rest of the chain up to the root.
	SigningCertPEM []byte
	// SigningKeyPEM is the issuing private key, PKCS#8.
	SigningKeyPEM []byte
	// DeviceTTL overrides the default certificate lifetime.
	DeviceTTL time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// New loads an Authority from PEM material.
func New(opts Options) (*Authority, error) {
	chain, err := parseCertChain(opts.SigningCertPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: parse signing certificate: %w", err)
	}
	if len(chain) == 0 {
		return nil, errors.New("ca: no certificate found in the signing PEM")
	}
	signing := chain[0]
	if !signing.IsCA {
		return nil, errors.New("ca: the signing certificate is not a CA certificate")
	}
	if signing.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("ca: the signing certificate lacks the certSign key usage")
	}

	key, err := parsePrivateKey(opts.SigningKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: parse signing key: %w", err)
	}
	// A mismatched key and certificate would produce certificates nothing can
	// verify. Catching it at startup beats discovering it when the first device
	// fails to connect.
	if !publicKeysEqual(key.Public(), signing.PublicKey) {
		return nil, errors.New("ca: the signing key does not match the signing certificate")
	}

	pool := x509.NewCertPool()
	for _, c := range chain {
		pool.AddCert(c)
	}

	ttl := opts.DeviceTTL
	if ttl <= 0 {
		ttl = DeviceTTL
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return &Authority{cert: signing, key: key, chain: chain, pool: pool, ttl: ttl, now: nowFn}, nil
}

// ChainPEM returns the issuing chain, for an agent to store and pin.
func (a *Authority) ChainPEM() []byte {
	var out []byte
	for _, c := range a.chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// Pool returns the verification pool for presented device certificates.
func (a *Authority) Pool() *x509.CertPool { return a.pool }

// DeviceTTL returns the configured device certificate lifetime.
func (a *Authority) DeviceTTL() time.Duration { return a.ttl }

// Issued is a signed device certificate.
type Issued struct {
	Certificate    *x509.Certificate
	CertificatePEM []byte
	// SerialDecimal is the serial as a decimal string. X.509 serials are up to
	// 20 bytes, far beyond int64, so it never passes through an integer type.
	SerialDecimal     string
	FingerprintSHA256 []byte
	PublicKeyDER      []byte
	NotBefore         time.Time
	NotAfter          time.Time
}

// Sign issues a device certificate for a validated CSR.
//
// Note what is NOT a parameter: any subject, SAN, or extension from the caller's
// request. deviceID and orgID come from server-side state — the device record
// the enrollment service created, and the organization the redeemed token
// belongs to. The CSR contributes only its public key.
func (a *Authority) Sign(csrDER []byte, deviceID, orgID uuid.UUID) (*Issued, error) {
	csr, err := ParseAndVerifyCSR(csrDER)
	if err != nil {
		return nil, err
	}
	if deviceID == uuid.Nil || orgID == uuid.Nil {
		return nil, errors.New("ca: device and organization ids are required")
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := a.now().UTC()
	// Backdated by a minute so a device whose clock is slightly behind does not
	// reject a certificate as not-yet-valid — which it cannot recover from
	// without a working connection, the very thing the certificate provides.
	notBefore := now.Add(-time.Minute)
	notAfter := now.Add(a.ttl)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			// The device id, from our records. Anything the CSR asked to be
			// called is discarded.
			CommonName:   deviceID.String(),
			Organization: []string{orgID.String()},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		// Client authentication only. Without this constraint a stolen device
		// certificate could be used to impersonate a *server* to other agents.
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},

		// Not a CA. Omitting this would let a device mint certificates for
		// every other device in the fleet.
		BasicConstraintsValid: true,
		IsCA:                  false,
		MaxPathLen:            -1,

		URIs: []*url.URL{spiffeURI(orgID, deviceID)},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, csr.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("ca: create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: re-parse issued certificate: %w", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("ca: marshal public key: %w", err)
	}
	fingerprint := sha256.Sum256(der)

	return &Issued{
		Certificate:       cert,
		CertificatePEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		SerialDecimal:     serial.String(),
		FingerprintSHA256: fingerprint[:],
		PublicKeyDER:      pubDER,
		NotBefore:         notBefore,
		NotAfter:          notAfter,
	}, nil
}

// Identity is what a presented device certificate proves.
type Identity struct {
	DeviceID          uuid.UUID
	OrgID             uuid.UUID
	FingerprintSHA256 []byte
	NotAfter          time.Time
}

// ServerTTL is how long an issued device-plane server certificate is valid.
//
// A year: this certificate is replaced by an operator rather than automatically,
// and a shorter life would mean a fleet-wide outage every time somebody forgot.
// Compromise is handled by re-issuing and restarting, not by waiting for expiry.
const ServerTTL = 365 * 24 * time.Hour

// IssueServerCertificate mints the TLS certificate the device plane presents.
//
// # Why this comes from the device CA and not from a public one
//
// An agent verifies the server against the CA bundle it was handed at
// enrollment — its own organization's CA — rather than against the system trust
// store. That is what stops a device being redirected to somebody else's server
// by a compromised trust store, a hostile DNS answer, or a mis-issued
// certificate from any of the hundreds of public CAs a browser trusts.
//
// The consequence is that the device plane's certificate has to be issued by
// this CA. Nothing else the agent will accept can be.
//
// # Why it is a separate method from Sign
//
// Sign issues *client* certificates and constrains them to client
// authentication, deliberately: a stolen device certificate must not let its
// holder impersonate a server to other agents. This is the mirror image, and
// keeping the two apart is what keeps that constraint honest — a single method
// with a flag is one wrong argument away from issuing a device a server
// certificate.
func (a *Authority) IssueServerCertificate(pub crypto.PublicKey, hosts []string, ttl time.Duration) (*Issued, error) {
	if len(hosts) == 0 {
		return nil, errors.New("ca: a server certificate needs at least one host")
	}
	if err := checkKeyStrength(pub); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = ServerTTL
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := a.now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(ttl),

		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// Server authentication only, for the reason in the doc comment above.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		BasicConstraintsValid: true,
		IsCA:                  false,
		MaxPathLen:            -1,
	}

	// Names go in the SAN, never in the CN. Every TLS implementation worth
	// using has ignored the CN for name verification since RFC 2818 was
	// superseded, and Go's has refused to look at it since 1.15.
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, host)
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, pub, a.key)
	if err != nil {
		return nil, fmt.Errorf("ca: create server certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("ca: re-parse issued certificate: %w", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("ca: marshal public key: %w", err)
	}
	fingerprint := sha256.Sum256(der)

	return &Issued{
		Certificate:       cert,
		CertificatePEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		SerialDecimal:     serial.String(),
		FingerprintSHA256: fingerprint[:],
		PublicKeyDER:      pubDER,
		NotBefore:         template.NotBefore,
		NotAfter:          template.NotAfter,
	}, nil
}

// IdentityFromChain extracts the device identity from a verified peer chain.
//
// It reads the SPIFFE SAN rather than the Common Name. The CN is a legacy field
// with no structure and no uniqueness guarantee; the URI SAN is what modern TLS
// stacks and every policy engine actually look at, and it carries the tenant as
// well as the device.
//
// The caller must already have verified the chain — this does not re-verify it.
// It does check the issuer, so a certificate from a different CA that somehow
// reached here is refused rather than trusted.
func (a *Authority) IdentityFromChain(chain []*x509.Certificate) (*Identity, error) {
	if len(chain) == 0 {
		return nil, ErrNoDeviceIdentity
	}
	leaf := chain[0]

	if leaf.IsCA {
		return nil, ErrNotClientCert
	}
	if !hasClientAuth(leaf) {
		return nil, ErrNotClientCert
	}
	if err := leaf.CheckSignatureFrom(a.cert); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWrongIssuer, err)
	}

	deviceID, orgID, err := parseSPIFFE(leaf)
	if err != nil {
		return nil, err
	}

	fingerprint := sha256.Sum256(leaf.Raw)
	return &Identity{
		DeviceID:          deviceID,
		OrgID:             orgID,
		FingerprintSHA256: fingerprint[:],
		NotAfter:          leaf.NotAfter,
	}, nil
}

// ParseAndVerifyCSR parses a DER certificate request and checks it thoroughly.
//
// The signature check is not optional and not merely a formality: a CSR is
// self-signed, and verifying it is the only evidence that the requester holds
// the private key for the public key inside. Skipping it would let anyone
// enrol a device using a public key they had merely observed — and then the
// certificate would be useless to them but valid, which is a confusing and
// exploitable state.
func ParseAndVerifyCSR(der []byte) (*x509.CertificateRequest, error) {
	if len(der) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrBadCSR)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCSRSignature, err)
	}
	if err := checkKeyStrength(csr.PublicKey); err != nil {
		return nil, err
	}
	return csr, nil
}

// checkKeyStrength refuses keys below current guidance.
//
// A device that generates a weak key gets a clear failure at enrollment rather
// than a certificate that is technically valid and practically forgeable.
func checkKeyStrength(pub crypto.PublicKey) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		default:
			// P-224 and anything non-NIST land here.
			return fmt.Errorf("%w: unsupported elliptic curve", ErrWeakKey)
		}
	case ed25519.PublicKey:
		return nil
	case *rsa.PublicKey:
		if k.N.BitLen() < 3072 {
			return fmt.Errorf("%w: RSA key is %d bits, minimum is 3072", ErrWeakKey, k.N.BitLen())
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported key type %T", ErrWeakKey, pub)
	}
}

// randomSerial returns a cryptographically random 128-bit serial.
//
// Random rather than sequential: a sequential serial discloses how many
// certificates have been issued — and therefore roughly how many devices exist —
// to anyone who obtains one, and makes the next serial predictable.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("ca: generate serial: %w", err)
	}
	// RFC 5280 requires a positive serial.
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func hasClientAuth(cert *x509.Certificate) bool {
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if e, ok := a.(equaler); ok {
		return e.Equal(b)
	}
	return false
}

func parseCertChain(pemBytes []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, errors.New("no CERTIFICATE block found")
	}
	return out, nil
}

func parsePrivateKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Fall back to the older encodings, which is what openssl still emits
		// by default in some versions.
		if ec, ecErr := x509.ParseECPrivateKey(block.Bytes); ecErr == nil {
			return ec, nil
		}
		if rsaKey, rsaErr := x509.ParsePKCS1PrivateKey(block.Bytes); rsaErr == nil {
			return rsaKey, nil
		}
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("key of type %T cannot sign", key)
	}
	return signer, nil
}
