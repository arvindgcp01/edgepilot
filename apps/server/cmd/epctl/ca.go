package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
)

var caBoolFlags = map[string]bool{"json": true, "force": true}

// caCmd manages the device certificate authority.
func caCmd(_ context.Context, args []string, stdout, stderr *os.File) int {
	sub, positional := splitSub(args, caBoolFlags)
	switch sub {
	case "init":
		return caInit(positional, args, stdout, stderr)
	case "server-cert":
		return caServerCert(args, stdout, stderr)
	case "":
		fmt.Fprintln(stderr, "epctl ca: expected a subcommand (init or server-cert)")
		return 2
	default:
		fmt.Fprintf(stderr, "epctl ca: unknown subcommand %q\n", sub)
		return 2
	}
}

// caInit generates the CA hierarchy and writes it to disk.
func caInit(_ []string, rawArgs []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl ca init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dir     = fs.String("dir", "./secrets/ca", "directory to write CA material into")
		orgName = fs.String("org", "EdgePilot", "organization name recorded in the CA subject")
		rootTTL = fs.Duration("root-ttl", ca.RootTTL, "root certificate lifetime")
		intTTL  = fs.Duration("intermediate-ttl", ca.IntermediateTTL, "intermediate certificate lifetime")
		force   = fs.Bool("force", false, "overwrite existing CA material")
	)

	flagArgs, _ := partitionArgs(rawArgs, caBoolFlags)
	// Drop the "init" positional, which partitionArgs leaves in the flag list
	// only if it were flag-shaped; it is not.
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}

	paths := map[string]string{
		"root certificate":         filepath.Join(*dir, "root.crt"),
		"root key":                 filepath.Join(*dir, "root.key"),
		"intermediate certificate": filepath.Join(*dir, "intermediate.crt"),
		"intermediate key":         filepath.Join(*dir, "intermediate.key"),
		"chain":                    filepath.Join(*dir, "chain.crt"),
	}

	// Overwriting a CA orphans every certificate issued under it — the whole
	// fleet would have to re-enrol. Refuse unless told explicitly.
	if !*force {
		for name, path := range paths {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(stderr,
					"epctl ca init: %s already exists at %s\n\n"+
						"Overwriting a CA orphans every certificate issued under it, and every\n"+
						"device in the fleet would have to re-enrol. Pass -force if that is what\n"+
						"you intend.\n", name, path)
				return 1
			}
		}
	}

	hierarchy, err := ca.Generate(ca.GenerateOptions{
		Organization:    *orgName,
		RootTTL:         *rootTTL,
		IntermediateTTL: *intTTL,
	})
	if err != nil {
		fmt.Fprintf(stderr, "epctl ca init: %v\n", err)
		return 1
	}

	// 0700: the directory holds private keys, and a world-readable parent makes
	// the file permissions below irrelevant.
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "epctl ca init: create %s: %v\n", *dir, err)
		return 1
	}

	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{paths["root certificate"], hierarchy.RootCertPEM, 0o644},
		{paths["root key"], hierarchy.RootKeyPEM, 0o600},
		{paths["intermediate certificate"], hierarchy.IntermediateCertPEM, 0o644},
		{paths["intermediate key"], hierarchy.IntermediateKeyPEM, 0o600},
		{paths["chain"], hierarchy.ChainPEM, 0o644},
	}
	for _, f := range files {
		// WriteFile does not change the mode of an existing file, so an
		// overwrite with -force could leave a key readable. Remove first.
		_ = os.Remove(f.path)
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			fmt.Fprintf(stderr, "epctl ca init: write %s: %v\n", f.path, err)
			return 1
		}
	}

	fmt.Fprintf(stdout, "Certificate authority written to %s\n\n", *dir)
	fmt.Fprintf(stdout, "  root.crt          %s valid until %s\n", "public ", fmtDate(time.Now().Add(*rootTTL)))
	fmt.Fprintf(stdout, "  root.key          PRIVATE, mode 0600\n")
	fmt.Fprintf(stdout, "  intermediate.crt  public,  valid until %s\n", fmtDate(time.Now().Add(*intTTL)))
	fmt.Fprintf(stdout, "  intermediate.key  PRIVATE, mode 0600\n")
	fmt.Fprintf(stdout, "  chain.crt         intermediate + root, for EP_CA_CERT\n")

	fmt.Fprint(stdout, `
Configure the server with the INTERMEDIATE, not the root:

    EP_CA_CERT=`+paths["chain"]+`
    EP_CA_KEY=`+paths["intermediate key"]+`

Then move root.key offline — a hardware token, or an encrypted volume that is
not attached to this machine — and delete it from here.

The intermediate lives on a server reachable from the internet, so it is the key
that can plausibly be stolen. If it is, you replace it and devices re-enrol. If
the root is stolen there is nothing to do but rebuild the fleet, which is the
whole reason it should never be online.
`)
	return 0
}

func fmtDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

// splitSub returns the first positional argument and the rest.
func splitSub(args []string, boolFlags map[string]bool) (string, []string) {
	_, positional := partitionArgs(args, boolFlags)
	if len(positional) == 0 {
		return "", nil
	}
	return positional[0], positional[1:]
}

// enrollTokenBoolFlags are the value-less flags for enrollment-token creation.
var enrollTokenBoolFlags = map[string]bool{"json": true, "auto-approve": true}

func fmtLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}
