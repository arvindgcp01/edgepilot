package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
)

// caServerCert issues the TLS certificate the device plane presents on :9443.
//
// # Why this cannot be a public certificate
//
// An agent verifies the server against the CA bundle it received at enrollment —
// its own organization's CA — rather than against the system trust store. That is
// what stops a device being redirected to another server by a hostile DNS answer
// or a mis-issued certificate from any of the hundreds of CAs a browser trusts.
//
// The consequence is that the device plane's certificate must be issued by this
// CA. A certificate from Let's Encrypt works for the dashboard on :443 and is
// useless on :9443.
//
// # Why this signs with the ROOT and not the intermediate
//
// The intermediate is deliberately constrained to `clientAuth`: even holding its
// key, an attacker cannot mint a certificate valid for server authentication and
// impersonate the control plane to the fleet. That property is the reason the
// intermediate can live on an internet-facing machine at all.
//
// It also means the intermediate cannot issue this certificate. X.509 extended
// key usage chains — a leaf claiming `serverAuth` under a CA restricted to
// `clientAuth` fails verification, and it fails at the agent, in the field.
//
// So this is a root operation. It needs `root.key`, which `epctl ca init` tells
// you to take offline, and it is run about once a year. That is the trade the
// hierarchy was designed to make: routine device issuance never touches the
// root, and the one certificate that could be used to impersonate the server
// cannot be minted without it.
func caServerCert(rawArgs []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl ca server-cert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dir   = fs.String("dir", "./secrets/ca", "directory holding the CA material")
		out   = fs.String("out", "", "directory to write the server certificate into (default: -dir)")
		hosts = fs.String("hosts", "",
			"comma-separated DNS names and IP addresses agents will dial, e.g. edgepilot.example.com,10.0.0.4")
		ttl   = fs.Duration("ttl", ca.ServerTTL, "certificate lifetime")
		force = fs.Bool("force", false, "overwrite an existing server certificate")
	)

	flagArgs, _ := partitionArgs(rawArgs, caBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}

	names := splitHosts(*hosts)
	if len(names) == 0 {
		fmt.Fprintln(stderr, "epctl ca server-cert: -hosts is required\n\n"+
			"It must list every name and address an agent will dial, because the\n"+
			"agent verifies them. A certificate for the wrong name fails the\n"+
			"handshake, which looks to an operator like the fleet going offline.")
		return 2
	}

	if *out == "" {
		*out = *dir
	}
	certPath := filepath.Join(*out, "server.crt")
	keyPath := filepath.Join(*out, "server.key")

	if !*force {
		for _, path := range []string{certPath, keyPath} {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(stderr, "epctl ca server-cert: %s already exists; "+
					"pass -force to replace it\n\n"+
					"Replacing it is safe — agents verify the CA, not this "+
					"certificate — but the server must be restarted to pick it up.\n", path)
				return 1
			}
		}
	}

	rootCertPEM, err := os.ReadFile(filepath.Join(*dir, "root.crt"))
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca server-cert: read the root certificate: %v\n\n"+
			"Run `epctl ca init` first.\n", err)
		return 1
	}
	rootKeyPEM, err := os.ReadFile(filepath.Join(*dir, "root.key"))
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca server-cert: read the root key: %v\n\n"+
			"This certificate must be signed by the root. The intermediate is\n"+
			"restricted to client authentication on purpose — that is what stops a\n"+
			"stolen intermediate key being used to impersonate this server to the\n"+
			"fleet — so it cannot issue a server certificate.\n\n"+
			"Bring root.key back from wherever it is stored, run this, and put it\n"+
			"away again. It is needed about once a year.\n", err)
		return 1
	}

	authority, err := ca.New(ca.Options{SigningCertPEM: rootCertPEM, SigningKeyPEM: rootKeyPEM})
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca server-cert: %v\n", err)
		return 1
	}

	// P-256, matching the CA and the device keys: one curve across the system,
	// and the handshake cost matters when ten thousand agents reconnect at once.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca server-cert: generate key: %v\n", err)
		return 1
	}

	issued, err := authority.IssueServerCertificate(key.Public(), names, *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca server-cert: %v\n", err)
		return 1
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca server-cert: encode key: %v\n", err)
		return 1
	}

	// The leaf alone. It is signed directly by the root, which every agent
	// already holds in the bundle it received at enrollment, so there is no
	// intermediate to supply — and appending one that did not sign it would
	// only confuse a client building the path.
	bundle := issued.CertificatePEM

	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{certPath, bundle, 0o644},
		{keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
	}
	for _, f := range files {
		// WriteFile leaves an existing file's mode alone, so an overwrite could
		// leave the key readable. Remove first.
		_ = os.Remove(f.path)
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			fmt.Fprintf(stderr, "epctl ca server-cert: write %s: %v\n", f.path, err)
			return 1
		}
	}

	fmt.Fprintf(stdout, "Device-plane certificate written to %s\n\n", *out)
	fmt.Fprintf(stdout, "  server.crt  public, valid until %s\n", fmtDate(issued.NotAfter))
	fmt.Fprintf(stdout, "  server.key  PRIVATE, mode 0600\n\n")
	fmt.Fprintf(stdout, "  names       %s\n", strings.Join(names, ", "))
	fmt.Fprintf(stdout, "  serial      %s\n\n", issued.SerialDecimal)

	fmt.Fprintf(stdout, `Configure the server with:

    EP_GRPC_CERT_FILE=%s
    EP_GRPC_KEY_FILE=%s

Agents verify this certificate against the CA bundle they received at
enrollment, not against the system trust store. A public certificate will not
work here, and this certificate will not work for the dashboard on :443 — those
are two different trust decisions and they need two different certificates.

This was signed by the ROOT key. Put root.key back into offline storage now; it
is not needed again until this certificate expires.
`, certPath, keyPath)
	return 0
}

// splitHosts parses the comma-separated -hosts value.
func splitHosts(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
