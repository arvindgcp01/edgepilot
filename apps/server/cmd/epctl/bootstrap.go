package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
	"github.com/visionify/edgepilot/apps/server/internal/platform/password"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
)

// bootstrapBoolFlags are the value-less flags for the bootstrap subcommand.
var bootstrapBoolFlags = map[string]bool{"json": true, "yes": true}

// bootstrapCmd creates the first organization and its administrator.
//
// This exists to break a chicken-and-egg problem: creating a user requires an
// authenticated admin, and there is no admin until one is created. Doing it
// through a CLI on the server rather than through an unauthenticated HTTP
// endpoint means the bootstrap path cannot be reached from the internet — an
// "initial setup" endpoint that forgets to disable itself after first use is a
// well-travelled way to hand over an entire platform.
func bootstrapCmd(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn      = dsnFlag(fs)
		orgName  = fs.String("org", "", "organization name (required)")
		slug     = fs.String("slug", "", "organization slug (derived from -org when omitted)")
		email    = fs.String("email", "", "administrator email address (required)")
		fullName = fs.String("name", "", "administrator full name")
		pwFlag   = fs.String("password", "", "administrator password; generated and printed when omitted")
		logLevel = fs.String("log-level", envOr("EP_LOG_LEVEL", "warn"), "debug | info | warn | error")
		asJSON   = fs.Bool("json", false, "emit machine-readable output")
	)

	flagArgs, _ := partitionArgs(args, bootstrapBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	*dsn = resolveDSN(*dsn)

	switch {
	case strings.TrimSpace(*dsn) == "":
		fmt.Fprintln(stderr, "epctl bootstrap: EP_DATABASE_URL is not set (or pass -database-url)")
		return 2
	case strings.TrimSpace(*orgName) == "":
		fmt.Fprintln(stderr, "epctl bootstrap: -org is required")
		return 2
	case strings.TrimSpace(*email) == "":
		fmt.Fprintln(stderr, "epctl bootstrap: -email is required")
		return 2
	}

	if *slug == "" {
		*slug = slugify(*orgName)
	}
	if *fullName == "" {
		*fullName = *email
	}

	// A generated password is preferable to one typed on a command line, where
	// it lands in shell history and in the process table for anyone running ps.
	generated := false
	pw := *pwFlag
	if pw == "" {
		var err error
		if pw, err = generatePassword(); err != nil {
			fmt.Fprintf(stderr, "epctl bootstrap: %v\n", err)
			return 1
		}
		generated = true
	}
	if err := password.ValidateLength(pw); err != nil {
		fmt.Fprintf(stderr, "epctl bootstrap: %v\n", err)
		return 2
	}

	log := logging.New(logging.Options{Level: *logLevel, Format: "text", Output: stderr})

	store, err := postgres.New(ctx, postgres.Options{DatabaseURL: *dsn, MaxConns: 4, Logger: log})
	if err != nil {
		fmt.Fprintf(stderr, "epctl bootstrap: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	hasher, err := password.New(password.DefaultParams())
	if err != nil {
		fmt.Fprintf(stderr, "epctl bootstrap: %v\n", err)
		return 1
	}
	hash, err := hasher.Hash(pw)
	if err != nil {
		fmt.Fprintf(stderr, "epctl bootstrap: %v\n", err)
		return 1
	}

	var (
		org  *domain.Organization
		user *domain.User
	)

	// One transaction: an organization with no administrator is unusable and
	// would have to be cleaned up by hand before a retry could succeed.
	err = store.InTx(ctx, func(tx domain.Store) error {
		org, err = tx.Organizations().Create(ctx, domain.CreateOrganizationInput{
			Name: *orgName,
			Slug: *slug,
		})
		if err != nil {
			return err
		}

		scoped := domain.WithOrg(ctx, org.ID)
		user, err = tx.Users().Create(scoped, domain.CreateUserInput{
			OrgID:     org.ID,
			Email:     *email,
			FullName:  *fullName,
			RoleSlugs: []string{domain.RoleAdmin},
		}, hash)
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// The caller-facing message rather than the whole chain: the driver
			// text underneath names a constraint, which is noise to an operator
			// who just wants to know this deployment is already set up.
			message := err.Error()
			if d, ok := domain.AsError(err); ok && d.Message != "" {
				message = d.Message
			}
			fmt.Fprintf(stderr,
				"epctl bootstrap: %s\n\nThis looks like an already-bootstrapped deployment. "+
					"Add further organizations through the API.\n", message)
			return 1
		}
		fmt.Fprintf(stderr, "epctl bootstrap: %v\n", err)
		return 1
	}

	if *asJSON {
		// The password is included here because the caller explicitly asked for
		// machine-readable output, which means they intend to capture it. It is
		// never written to the log, only to stdout.
		fmt.Fprintf(stdout, `{"organization_id":%q,"organization_slug":%q,"user_id":%q,"email":%q,"password":%q,"generated_password":%t}%s`,
			org.ID, org.Slug, user.ID, user.Email, pw, generated, "\n")
		return 0
	}

	fmt.Fprintf(stdout, "Organization  %s (%s)\n", org.Name, org.Slug)
	fmt.Fprintf(stdout, "  id          %s\n", org.ID)
	fmt.Fprintf(stdout, "Administrator %s\n", user.Email)
	fmt.Fprintf(stdout, "  id          %s\n", user.ID)
	fmt.Fprintf(stdout, "  roles       %s\n", strings.Join(user.Roles, ", "))
	if generated {
		fmt.Fprintf(stdout, "  password    %s\n", pw)
		fmt.Fprintln(stdout, "\nThis password is shown once and is not recoverable. "+
			"Store it in a password manager and change it after first sign-in.")
	}
	return 0
}

// generatePassword returns a high-entropy password.
//
// 32 bytes of base64: ~192 bits, which is far beyond anything a human would
// choose and costs nothing since it goes straight into a password manager.
func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// slugify derives an organization slug from its name, matching the CHECK
// constraint: lowercase letters, digits, and hyphens, starting alphanumeric.
func slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	if len(slug) < 2 {
		// Falls back to something valid rather than failing: the operator can
		// override with -slug, and a rejected CHECK constraint here would be a
		// confusing way to learn that.
		slug = fmt.Sprintf("org-%d", time.Now().Unix())
	}
	return slug
}
