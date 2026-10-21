package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
)

// enrollTokenCmd mints an enrollment token.
//
// Available from the CLI as well as the API because the first token has to be
// created before anyone has signed in to create one through the dashboard —
// the same chicken-and-egg problem bootstrap solves for users.
func enrollTokenCmd(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl enrollment-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn         = dsnFlag(fs)
		orgSlug     = fs.String("org", "", "organization slug (required)")
		description = fs.String("description", "", "what this token is for")
		ttl         = fs.Duration("ttl", 24*time.Hour, "how long the token remains usable")
		maxUses     = fs.Int("max-uses", 0, "maximum enrollments; 0 means unlimited within the TTL")
		autoApprove = fs.Bool("auto-approve", false,
			"approve enrolled devices immediately instead of leaving them pending")
		labelList = fs.String("labels", "", "comma-separated key=value labels stamped onto enrolled devices")
		logLevel  = fs.String("log-level", envOr("EP_LOG_LEVEL", "warn"), "debug | info | warn | error")
		asJSON    = fs.Bool("json", false, "emit machine-readable output")
	)

	flagArgs, _ := partitionArgs(args, enrollTokenBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	*dsn = resolveDSN(*dsn)

	switch {
	case strings.TrimSpace(*dsn) == "":
		fmt.Fprintln(stderr, "epctl enrollment-token: EP_DATABASE_URL is not set (or pass -database-url)")
		return 2
	case strings.TrimSpace(*orgSlug) == "":
		fmt.Fprintln(stderr, "epctl enrollment-token: -org is required")
		return 2
	}

	labels, err := parseLabels(*labelList)
	if err != nil {
		fmt.Fprintf(stderr, "epctl enrollment-token: %v\n", err)
		return 2
	}

	log := logging.New(logging.Options{Level: *logLevel, Format: "text", Output: stderr})
	store, err := postgres.New(ctx, postgres.Options{DatabaseURL: *dsn, MaxConns: 4, Logger: log})
	if err != nil {
		fmt.Fprintf(stderr, "epctl enrollment-token: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	org, err := store.Organizations().GetBySlug(ctx, *orgSlug)
	if err != nil {
		fmt.Fprintf(stderr, "epctl enrollment-token: no organization with slug %q\n", *orgSlug)
		return 1
	}

	plaintext, hash, prefix, err := token.NewEnrollmentToken()
	if err != nil {
		fmt.Fprintf(stderr, "epctl enrollment-token: %v\n", err)
		return 1
	}

	var maxUsesPtr *int
	if *maxUses > 0 {
		maxUsesPtr = maxUses
	}

	created, err := store.EnrollmentTokens().Create(
		domain.WithOrg(ctx, org.ID),
		domain.CreateEnrollmentTokenInput{
			OrgID:       org.ID,
			Description: *description,
			MaxUses:     maxUsesPtr,
			AutoApprove: *autoApprove,
			Labels:      labels,
			ExpiresAt:   time.Now().Add(*ttl),
		}, hash, prefix)
	if err != nil {
		fmt.Fprintf(stderr, "epctl enrollment-token: %v\n", err)
		return 1
	}

	if *asJSON {
		fmt.Fprintf(stdout, `{"id":%q,"token":%q,"organization":%q,"expires_at":%q,"auto_approve":%t}%s`,
			created.ID, plaintext, org.Slug, created.ExpiresAt.Format(time.RFC3339), created.AutoApprove, "\n")
		return 0
	}

	fmt.Fprintf(stdout, "Enrollment token for %s\n\n", org.Name)
	fmt.Fprintf(stdout, "  %s\n\n", plaintext)
	fmt.Fprintf(stdout, "  id            %s\n", created.ID)
	fmt.Fprintf(stdout, "  expires       %s\n", created.ExpiresAt.UTC().Format(time.RFC3339))
	if maxUsesPtr != nil {
		fmt.Fprintf(stdout, "  max uses      %d\n", *maxUsesPtr)
	} else {
		fmt.Fprintf(stdout, "  max uses      unlimited within the TTL\n")
	}
	fmt.Fprintf(stdout, "  auto-approve  %t\n", created.AutoApprove)
	fmt.Fprintf(stdout, "  labels        %s\n", fmtLabels(created.Labels))

	fmt.Fprint(stdout, `
Shown once — only its SHA-256 is stored, so it cannot be recovered.

Install it on the Mac:

    sudo edgepilot-agent enroll --server <host> --token <token>
`)
	if created.AutoApprove {
		fmt.Fprint(stdout, `
This token auto-approves. Devices enrolled with it receive commands immediately,
with no human in the loop — so anyone who obtains it can put a machine into the
fleet that is trusted from the moment it connects. Prefer leaving auto-approve
off for anything reaching a customer site.
`)
	}
	return 0
}

// parseLabels parses a comma-separated key=value list.
func parseLabels(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("label %q is not in key=value form", pair)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("label %q has an empty key", pair)
		}
		out[key] = value
	}
	return out, nil
}
