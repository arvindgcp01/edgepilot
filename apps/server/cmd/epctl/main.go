// Command epctl is EdgePilot's administrative CLI.
//
// It carries the schema it was built with (see internal/migrations), so
// `epctl migrate up` from a given release can only ever apply that release's
// migrations. Running it from a container image, a release archive, or `go run`
// gives the same result.
//
// Subcommands are added as the phases land. Today: migrate, version.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
	"github.com/visionify/edgepilot/apps/server/internal/platform/migrate"
	"github.com/visionify/edgepilot/apps/server/migrations"
)

// Set at build time with -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	// Ctrl-C stops after the current migration rather than mid-statement, which
	// is the difference between "nothing to clean up" and a dirty database.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "migrate":
		return migrateCmd(ctx, args[1:], stdout, stderr)
	case "bootstrap":
		return bootstrapCmd(ctx, args[1:], stdout, stderr)
	case "ca":
		return caCmd(ctx, args[1:], stdout, stderr)
	case "enrollment-token":
		return enrollTokenCmd(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "epctl %s (%s)\n", version, commit)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "epctl: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `epctl — EdgePilot administration

Usage:
  epctl migrate up                 Apply every pending migration
  epctl migrate down <n>           Reverse the most recent n migrations
  epctl migrate to <version>       Migrate up or down to a specific version
  epctl migrate status             Show the current schema state
  epctl migrate force <version>    Record a version without applying SQL (recovery)

  epctl bootstrap -org NAME -email ADDR
                                   Create the first organization and its admin.
                                   Generates and prints a password when -password
                                   is omitted, which is preferable to typing one
                                   into shell history.

  epctl ca init                    Generate the device certificate authority
  epctl ca server-cert -hosts NAMES
                                   Issue the TLS certificate the device plane
                                   presents on :9443. Agents verify it against
                                   the CA they were enrolled with, so a public
                                   certificate cannot be used here.

  epctl enrollment-token -org SLUG
                                   Mint an enrollment token. Printed once; only
                                   its SHA-256 is stored.

  epctl version                    Print the build version

Environment:
  EP_DATABASE_URL   postgres:// DSN (required; override with -database-url)
  EP_LOG_LEVEL      debug | info | warn | error (default info)

Exit codes:
  0  success
  1  failure
  2  usage error
  3  migrations pending (from "migrate status -check")
`)
}

func migrateCmd(ctx context.Context, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("epctl migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn      = fs.String("database-url", os.Getenv("EP_DATABASE_URL"), "postgres:// DSN")
		logLevel = fs.String("log-level", envOr("EP_LOG_LEVEL", "info"), "debug | info | warn | error")
		asJSON   = fs.Bool("json", false, "emit machine-readable output")
		check    = fs.Bool("check", false, "status only: exit 3 when migrations are pending")
		timeout  = fs.Duration("timeout", 10*time.Minute, "abort if the run takes longer than this")
		yes      = fs.Bool("yes", false, "skip the confirmation prompt for destructive operations")
	)

	// Go's flag package stops parsing at the first non-flag argument, so
	// `migrate down 1 -yes` would parse no flags at all and silently ignore
	// -yes — on the one command where that flag suppresses a confirmation
	// prompt. Separating flags from positionals first makes every argument
	// order behave the same.
	flagArgs, positional := partitionArgs(args, migrateBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}

	var sub string
	if len(positional) > 0 {
		sub, positional = positional[0], positional[1:]
	}
	if sub == "" {
		fmt.Fprintln(stderr, "epctl migrate: expected up, down, to, status, or force")
		return 2
	}
	if strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(stderr, "epctl migrate: EP_DATABASE_URL is not set (or pass -database-url)")
		return 2
	}

	log := logging.New(logging.Options{Level: *logLevel, Format: "text", Output: stderr})

	runner, err := migrate.New(migrate.Options{
		DatabaseURL: *dsn,
		Source:      migrations.FS,
		Logger:      log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
		return 1
	}
	defer func() {
		if closeErr := runner.Close(); closeErr != nil {
			log.Warn("closing migration runner", "error", closeErr)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	switch sub {
	case "up":
		return report(stderr, runner.Up(ctx))

	case "status":
		return statusCmd(ctx, runner, stdout, stderr, *asJSON, *check)

	case "down":
		n, err := requireIntArg(positional, "down <n>")
		if err != nil {
			fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
			return 2
		}
		// Down-migrations drop tables. Ask, unless explicitly told not to.
		if !*yes && !confirm(stdout, stderr,
			fmt.Sprintf("Reverse the most recent %d migration(s)? This destroys data.", n)) {
			fmt.Fprintln(stderr, "aborted")
			return 1
		}
		return report(stderr, runner.Down(ctx, n))

	case "to":
		v, err := requireIntArg(positional, "to <version>")
		if err != nil {
			fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
			return 2
		}
		if v < 0 {
			fmt.Fprintln(stderr, "epctl migrate: version must not be negative")
			return 2
		}
		if !*yes && !confirm(stdout, stderr,
			fmt.Sprintf("Migrate to version %d? This may destroy data.", v)) {
			fmt.Fprintln(stderr, "aborted")
			return 1
		}
		return report(stderr, runner.To(ctx, uint(v)))

	case "force":
		v, err := requireIntArg(positional, "force <version>")
		if err != nil {
			fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
			return 2
		}
		fmt.Fprintln(stderr,
			"WARNING: force records a version without running any SQL. "+
				"Confirm what the failed migration actually applied first.")
		if !*yes && !confirm(stdout, stderr, fmt.Sprintf("Force the recorded version to %d?", v)) {
			fmt.Fprintln(stderr, "aborted")
			return 1
		}
		return report(stderr, runner.Force(ctx, v))

	default:
		fmt.Fprintf(stderr, "epctl migrate: unknown subcommand %q\n", sub)
		return 2
	}
}

func statusCmd(ctx context.Context, r *migrate.Runner, stdout, stderr *os.File, asJSON, check bool) int {
	st, err := r.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"version":    st.Version,
			"dirty":      st.Dirty,
			"available":  st.Available,
			"pending":    st.Pending,
			"up_to_date": st.UpToDate(),
		}); err != nil {
			fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
			return 1
		}
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "applied version\t%d\n", st.Version)
		fmt.Fprintf(tw, "dirty\t%t\n", st.Dirty)
		fmt.Fprintf(tw, "migrations available\t%d\n", len(st.Available))
		fmt.Fprintf(tw, "migrations pending\t%d\n", len(st.Pending))
		if len(st.Pending) > 0 {
			fmt.Fprintf(tw, "pending versions\t%s\n", joinUints(st.Pending))
		}
		if err := tw.Flush(); err != nil {
			fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
			return 1
		}
	}

	if st.Dirty {
		fmt.Fprintln(stderr,
			"\nThe database is dirty: a migration failed part-way. Inspect what it applied, "+
				"then use `epctl migrate force <version>`.")
		return 1
	}
	// A distinct exit code lets a readiness probe or a deploy gate ask "is the
	// schema current?" without parsing output.
	if check && len(st.Pending) > 0 {
		return 3
	}
	return 0
}

func report(stderr *os.File, err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(stderr, "epctl migrate: timed out; the migration may still be running")
		return 1
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "epctl migrate: interrupted; stopped cleanly between migrations")
		return 1
	}
	fmt.Fprintf(stderr, "epctl migrate: %v\n", err)
	return 1
}

// migrateBoolFlags lists the flags that take no value, so partitionArgs knows
// not to swallow the argument after them.
var migrateBoolFlags = map[string]bool{"json": true, "check": true, "yes": true}

// partitionArgs separates flags from positional arguments, so argument order
// does not change behaviour.
//
// This exists because the flag package treats the first positional argument as
// the end of the flags. That is a reasonable default for a single-command tool
// and a trap for a subcommand tool: `migrate down 1 -yes` would parse zero
// flags and quietly skip the confirmation prompt that -yes was meant to answer.
func partitionArgs(args []string, boolFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			return flags, positional
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)

		// A value-taking flag written as `-name value` consumes the next
		// argument; `-name=value` and boolean flags do not.
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") || boolFlags[name] {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

func requireIntArg(args []string, form string) (int, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("expected %s", form)
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("expected %s, got %q", form, args[0])
	}
	return n, nil
}

// confirm prompts before a destructive operation.
//
// A non-interactive stdin declines rather than proceeding: a script that reaches
// this prompt did not intend to answer it, and silently continuing would drop
// tables. Detection uses x/term rather than checking for a character device,
// because /dev/null is a character device and would otherwise read as a
// terminal — turning `epctl migrate down 1 </dev/null` into an unattended
// destructive operation.
func confirm(stdout, stderr *os.File, question string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(stderr, "refusing to proceed without confirmation "+
			"(stdin is not a terminal; pass -yes to confirm explicitly)")
		return false
	}
	fmt.Fprintf(stdout, "%s [y/N] ", question)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func joinUints(vs []uint) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.FormatUint(uint64(v), 10)
	}
	return strings.Join(parts, ", ")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
