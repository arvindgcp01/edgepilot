package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"
)

// Hierarchy is a freshly generated CA.
//
// Two levels, not one. The intermediate key lives on a server that answers
// requests from the internet and is therefore the one that can plausibly be
// stolen; the root should go straight to offline storage after generation. If
// the intermediate is compromised, it is replaced and devices re-enrol. If the
// root is compromised, there is nothing to do but rebuild the fleet — which is
// why it should never be online in the first place.
type Hierarchy struct {
	RootCertPEM []byte
	RootKeyPEM  []byte

	IntermediateCertPEM []byte
	IntermediateKeyPEM  []byte

	// ChainPEM is the intermediate followed by the root, which is what the
	// server loads as its signing material.
	ChainPEM []byte
}

// GenerateOptions configures CA generation.
type GenerateOptions struct {
	// Organization names the CA in its subject, for a human reading it.
	Organization string
	// RootTTL and IntermediateTTL override the defaults.
	RootTTL         time.Duration
	IntermediateTTL time.Duration
	Now             func() time.Time
}

// Generate creates a root and an intermediate.
//
// ECDSA P-256 rather than RSA: signatures and certificates are much smaller,
// which matters on a TLS handshake that 10,000 agents perform repeatedly, and
// P-256 is universally supported. RSA-4096 would buy nothing here except
// slower handshakes.
func Generate(opts GenerateOptions) (*Hierarchy, error) {
	if opts.Organization == "" {
		opts.Organization = "EdgePilot"
	}
	if opts.RootTTL <= 0 {
		opts.RootTTL = RootTTL
	}
	if opts.IntermediateTTL <= 0 {
		opts.IntermediateTTL = IntermediateTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IntermediateTTL > opts.RootTTL {
		return nil, fmt.Errorf("ca: the intermediate cannot outlive the root (%s > %s)",
			opts.IntermediateTTL, opts.RootTTL)
	}
	now := opts.Now().UTC()

	// --- root ---------------------------------------------------------------

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: generate root key: %w", err)
	}
	rootSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: rootSerial,
		Subject: pkix.Name{
			CommonName:   opts.Organization + " Device Root CA",
			Organization: []string{opts.Organization},
		},
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(opts.RootTTL),

		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// The root signs intermediates, and an intermediate signs leaves. A
		// path length of 1 means a compromised intermediate cannot mint a
		// further CA below itself.
		MaxPathLen: 1,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("ca: create root certificate: %w", err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, fmt.Errorf("ca: re-parse root: %w", err)
	}

	// --- intermediate -------------------------------------------------------

	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca: generate intermediate key: %w", err)
	}
	interSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	interTemplate := &x509.Certificate{
		SerialNumber: interSerial,
		Subject: pkix.Name{
			CommonName:   opts.Organization + " Device Issuing CA",
			Organization: []string{opts.Organization},
		},
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(opts.IntermediateTTL),

		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// Zero: this CA may issue leaf certificates and nothing else.
		MaxPathLen:     0,
		MaxPathLenZero: true,

		// Constrain what this CA can ever issue. Even holding the intermediate
		// key, an attacker cannot mint a certificate valid for server
		// authentication and impersonate the control plane to agents.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	interDER, err := x509.CreateCertificate(rand.Reader, interTemplate, rootCert, &interKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("ca: create intermediate certificate: %w", err)
	}

	rootKeyPEM, err := encodeKey(rootKey)
	if err != nil {
		return nil, err
	}
	interKeyPEM, err := encodeKey(interKey)
	if err != nil {
		return nil, err
	}

	rootCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	interCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: interDER})

	return &Hierarchy{
		RootCertPEM:         rootCertPEM,
		RootKeyPEM:          rootKeyPEM,
		IntermediateCertPEM: interCertPEM,
		IntermediateKeyPEM:  interKeyPEM,
		// Leaf-most first, as TLS expects.
		ChainPEM: append(append([]byte{}, interCertPEM...), rootCertPEM...),
	}, nil
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("ca: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
