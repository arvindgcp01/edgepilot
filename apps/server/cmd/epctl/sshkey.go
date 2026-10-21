package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
)

var sshKeyBoolFlags = map[string]bool{"json": true}

// sshKeyCmd manages the public keys that may reach the bastion.
//
// A key here authenticates an operator to EdgePilot. It does not grant a shell:
// the Mac's own sshd still decides who may log in, and the operator's key has to
// be in the target account's authorized_keys as well. Two independent checks,
// which is what lets EdgePilot revoke fleet-wide access without ever holding a
// credential that is sufficient on its own.
func sshKeyCmd(ctx context.Context, args []string, stdout, stderr *os.File) int {
	sub, _ := splitSub(args, sshKeyBoolFlags)
	switch sub {
	case "add":
		return sshKeyAdd(ctx, args, stdout, stderr)
	case "list":
		return sshKeyList(ctx, args, stdout, stderr)
	case "":
		fmt.Fprintln(stderr, "epctl ssh-key: expected a subcommand (add or list)")
		return 2
	default:
		fmt.Fprintf(stderr, "epctl ssh-key: unknown subcommand %q\n", sub)
		return 2
	}
}

func sshKeyAdd(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl ssh-key add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn      = dsnFlag(fs)
		orgSlug  = fs.String("org", "", "organization slug (required)")
		name     = fs.String("name", "", "a label for this key (required)")
		keyFile  = fs.String("key-file", "", "path to a public key, e.g. ~/.ssh/id_ed25519.pub")
		keyText  = fs.String("key", "", "the public key itself, as an authorized_keys line")
		email    = fs.String("email", "", "owner's email address; omit for an org-wide service key")
		target   = fs.String("user", "", "the local account on each Mac this key logs into (required)")
		ttl      = fs.Duration("ttl", 0, "how long the key remains usable; 0 means no expiry")
		logLevel = fs.String("log-level", envOr("EP_LOG_LEVEL", "warn"), "debug | info | warn | error")
	)

	flagArgs, _ := partitionArgs(args, sshKeyBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	*dsn = resolveDSN(*dsn)

	switch {
	case strings.TrimSpace(*dsn) == "":
		fmt.Fprintln(stderr, "epctl ssh-key add: EP_DATABASE_URL is not set (or pass -database-url)")
		return 2
	case strings.TrimSpace(*orgSlug) == "":
		fmt.Fprintln(stderr, "epctl ssh-key add: -org is required")
		return 2
	case strings.TrimSpace(*name) == "":
		fmt.Fprintln(stderr, "epctl ssh-key add: -name is required")
		return 2
	case *keyFile == "" && *keyText == "":
		fmt.Fprintln(stderr, "epctl ssh-key add: pass -key-file or -key")
		return 2
	case strings.TrimSpace(*target) == "":
		// No default. EdgePilot does not create accounts, so this has to name
		// one that already exists on the Macs — and guessing which would install
		// keys somewhere nobody asked for.
		fmt.Fprintln(stderr, "epctl ssh-key add: -user is required\n\n"+
			"It names the existing local account on each Mac that this key logs\n"+
			"into, e.g. -user pallet. EdgePilot never creates accounts.")
		return 2
	}

	material := *keyText
	if *keyFile != "" {
		raw, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintf(stderr, "epctl ssh-key add: reading %s: %v\n", *keyFile, err)
			return 1
		}
		material = string(raw)
	}

	// Parsed rather than stored verbatim. A private key pasted here by mistake
	// would otherwise sit in the database looking like an authorization, and
	// the error at the bastion would come months later.
	parsed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(material)))
	if err != nil {
		fmt.Fprintf(stderr, "epctl ssh-key add: that is not an OpenSSH public key: %v\n\n"+
			"Expected one authorized_keys line, e.g. the contents of ~/.ssh/id_ed25519.pub.\n", err)
		return 1
	}

	keyType := parsed.Type()
	if keyType == ssh.KeyAlgoRSA {
		// The schema rejects it too; saying so here explains why rather than
		// surfacing a constraint violation.
		fmt.Fprintln(stderr, "epctl ssh-key add: RSA keys are not accepted.\n\n"+
			"Generate an Ed25519 key instead:\n\n    ssh-keygen -t ed25519")
		return 1
	}

	log := logging.New(logging.Options{Level: *logLevel, Format: "text", Output: stderr})
	store, err := postgres.New(ctx, postgres.Options{
		DatabaseURL: *dsn, MaxConns: 2, Logger: log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "epctl ssh-key add: connecting to the database: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	org, err := store.Organizations().GetBySlug(ctx, *orgSlug)
	if err != nil {
		fmt.Fprintf(stderr, "epctl ssh-key add: no organization with slug %q\n", *orgSlug)
		return 1
	}

	scoped := domain.WithOrg(ctx, org.ID)

	var ownerID *uuid.UUID
	if *email != "" {
		user, userErr := store.Users().GetByEmail(scoped, *email)
		if userErr != nil {
			fmt.Fprintf(stderr, "epctl ssh-key add: no user %q in %s\n", *email, org.Name)
			return 1
		}
		ownerID = &user.ID
	}

	var expiresAt *time.Time
	if *ttl > 0 {
		t := time.Now().Add(*ttl).UTC()
		expiresAt = &t
	}

	if *name == "" {
		*name = comment
	}

	key, err := store.SSHKeys().Create(scoped, domain.CreateSSHKeyInput{
		OrgID:             org.ID,
		OwnerUserID:       ownerID,
		Name:              *name,
		PublicKey:         string(ssh.MarshalAuthorizedKey(parsed)),
		KeyType:           keyType,
		FingerprintSHA256: ssh.FingerprintSHA256(parsed),
		Scope:             "org",
		TargetUsername:    strings.TrimSpace(*target),
		ExpiresAt:         expiresAt,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			fmt.Fprintln(stderr, "epctl ssh-key add: that key is already registered for this organization")
			return 1
		}
		fmt.Fprintf(stderr, "epctl ssh-key add: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Registered %q for %s\n\n", key.Name, org.Name)
	fmt.Fprintf(stdout, "  fingerprint  %s\n", key.FingerprintSHA256)
	fmt.Fprintf(stdout, "  type         %s\n", key.KeyType)
	fmt.Fprintf(stdout, "  logs in as   %s\n", key.TargetUsername)
	if expiresAt != nil {
		fmt.Fprintf(stdout, "  expires      %s\n", expiresAt.Format(time.RFC3339))
	} else {
		fmt.Fprintf(stdout, "  expires      never\n")
	}
	fmt.Fprintf(stdout, "\nEvery approved Mac in %s installs this key for %q on its next\n"+
		"connect. The account must already exist there — EdgePilot creates none.\n"+
		"Remote Login must also be on.\n\n"+
		"    ssh -J %s:2222 %s@<device-hostname>\n",
		org.Name, key.TargetUsername,
		envOr("EP_PUBLIC_HOST", "<bastion>"), key.TargetUsername)

	return 0
}

func sshKeyList(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl ssh-key list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn      = dsnFlag(fs)
		orgSlug  = fs.String("org", "", "organization slug (required)")
		logLevel = fs.String("log-level", envOr("EP_LOG_LEVEL", "warn"), "debug | info | warn | error")
	)

	flagArgs, _ := partitionArgs(args, sshKeyBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	*dsn = resolveDSN(*dsn)

	if strings.TrimSpace(*orgSlug) == "" {
		fmt.Fprintln(stderr, "epctl ssh-key list: -org is required")
		return 2
	}

	log := logging.New(logging.Options{Level: *logLevel, Format: "text", Output: stderr})
	store, err := postgres.New(ctx, postgres.Options{
		DatabaseURL: *dsn, MaxConns: 2, Logger: log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "epctl ssh-key list: connecting to the database: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	org, err := store.Organizations().GetBySlug(ctx, *orgSlug)
	if err != nil {
		fmt.Fprintf(stderr, "epctl ssh-key list: no organization with slug %q\n", *orgSlug)
		return 1
	}

	keys, err := store.SSHKeys().ListForOrg(domain.WithOrg(ctx, org.ID))
	if err != nil {
		fmt.Fprintf(stderr, "epctl ssh-key list: %v\n", err)
		return 1
	}
	if len(keys) == 0 {
		fmt.Fprintf(stdout, "No SSH keys registered for %s.\n", org.Name)
		return 0
	}

	now := time.Now()
	for _, k := range keys {
		state := "active"
		switch {
		case k.RevokedAt != nil:
			state = "revoked"
		case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
			state = "expired"
		}
		fmt.Fprintf(stdout, "%-20s  %-10s  %-12s  %s\n",
			k.Name, state, k.TargetUsername, k.FingerprintSHA256)
	}
	return 0
}
