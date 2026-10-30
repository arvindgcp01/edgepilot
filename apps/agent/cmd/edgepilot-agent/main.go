// Command edgepilot-agent is the macOS fleet agent.
//
// It runs as a root LaunchDaemon, enrolls itself on first boot, and then holds
// one mTLS gRPC stream open to the server for as long as the machine is up.
//
// Signals:
//
//	SIGTERM, SIGINT   shut down cleanly (launchd sends SIGTERM)
//	SIGHUP            reopen the log file after newsyslog rotates it
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/visionify/edgepilot/apps/agent/internal/client"
	"github.com/visionify/edgepilot/apps/agent/internal/collect"
	"github.com/visionify/edgepilot/apps/agent/internal/config"
	"github.com/visionify/edgepilot/apps/agent/internal/logging"
	"github.com/visionify/edgepilot/apps/agent/internal/supervisor"
)

// Set by the linker. See the build-agent target in the Makefile.
var (
	version = "dev"
	commit  = "none"
)

// Exit codes. launchd reads these, and an operator reading `launchctl print`
// should be able to tell "misconfigured" from "revoked" without opening a log.
const (
	exitOK = 0
	// exitConfig means the agent cannot start with the configuration it was
	// given. Restarting will not help until someone changes it.
	exitConfig = 78 // EX_CONFIG
	// exitTerminal means the server has told this device to stop: revoked,
	// decommissioned, or holding an expired certificate.
	exitTerminal = 77 // EX_NOPERM
	// exitFailure is everything else.
	exitFailure = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr *os.File) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Fprintf(stdout, "edgepilot-agent %s (%s)\n", version, commit)
			return exitOK
		case "metrics":
			// One health reading, as JSON, straight from the collector. This is
			// what an operator runs on a Mac whose dashboard tile says "No
			// metrics": it answers "can this machine be read at all" without
			// involving the server, the stream, or a log file.
			return printMetrics(stdout, stderr)
		case "-h", "-help", "--help", "help":
			usage(stdout)
			return exitOK
		default:
			fmt.Fprintf(stderr, "edgepilot-agent: unknown argument %q\n\n", args[0])
			usage(stderr)
			return exitConfig
		}
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		// Before the logger exists, so this goes straight to stderr — which
		// launchd captures.
		fmt.Fprintf(stderr, "edgepilot-agent: %v\n", err)
		return exitConfig
	}

	log, writer, err := logging.New(logging.Options{
		Path: cfg.LogFile, Level: cfg.LogLevel, Format: "text",
	})
	if err != nil {
		fmt.Fprintf(stderr, "edgepilot-agent: %v\n", err)
		return exitConfig
	}
	defer func() { _ = writer.Close() }()

	// SIGTERM and SIGINT stop the agent; launchd sends SIGTERM on `launchctl
	// unload` and at shutdown.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// SIGHUP is newsyslog telling the agent its log file has been rotated out
	// from under it.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := writer.Reopen(); err != nil {
				fmt.Fprintf(stderr, "edgepilot-agent: reopening the log: %v\n", err)
				continue
			}
			log.Info("reopened the log file after rotation")
		}
	}()

	sv, err := supervisor.New(supervisor.Options{
		Config: cfg, Logger: log, Version: version,
	})
	if err != nil {
		log.Error("the agent could not start", "error", err)
		return exitFailure
	}

	runErr := sv.Run(ctx)
	switch {
	case runErr == nil:
		log.Info("agent stopped")
		return exitOK

	case errors.Is(runErr, context.Canceled):
		log.Info("agent stopped")
		return exitOK

	case errors.Is(runErr, client.ErrRevoked),
		errors.Is(runErr, client.ErrDeleted),
		errors.Is(runErr, client.ErrCertificateExpired),
		errors.Is(runErr, supervisor.ErrEnrollmentImpossible):
		// launchd will restart the process; the restart will fail the same way.
		// A distinct exit code is what tells an operator that, rather than
		// leaving them to infer it from a restart loop.
		log.Error("the agent has stopped and will not recover on its own",
			"error", runErr)
		return exitTerminal

	default:
		log.Error("the agent stopped unexpectedly", "error", runErr)
		return exitFailure
	}
}

func printMetrics(stdout, stderr *os.File) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sample, err := collect.NewMetrics().Sample(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "edgepilot-agent: reading metrics failed: %v\n", err)
		return exitFailure
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sample); err != nil {
		fmt.Fprintf(stderr, "edgepilot-agent: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func usage(w *os.File) {
	fmt.Fprint(w, `edgepilot-agent — the EdgePilot macOS fleet agent

Usage:
  edgepilot-agent           run the agent (this is what launchd does)
  edgepilot-agent version   print the version and exit
  edgepilot-agent metrics   read one health sample from this Mac and print it as JSON

Configuration is environment-only. On first boot the agent needs:

  EP_SERVER_URL             the management server, e.g. https://edgepilot.example.com
  EP_ENROLL_TOKEN_FILE      a file holding the enrollment token
                            (default /etc/edgepilot/enroll)
  EP_ENROLL_TOKEN           the token itself, if not supplied through a file

Optional:

  EP_DEVICE_PLANE_ENDPOINT  override the endpoint enrollment returned
  EP_SERVER_CA_FILE         pin the CA that signed the server's TLS certificate
  EP_STATE_DIR              where the identity lives (default /var/db/edgepilot on macOS,
                            /var/lib/edgepilot on Linux)
  EP_SPOOL_DIR              the offline buffer (default <state dir>/spool)
  EP_SPOOL_MAX_BYTES        the buffer's ceiling (default 64 MiB)
  EP_LOG_FILE               where to log (default /var/log/edgepilot/agent.log)
  EP_LOG_LEVEL              debug | info | warn | error (default info)

Once enrolled, the agent needs none of the enrollment settings: its
configuration comes from the server as policy.

Signals:
  SIGTERM, SIGINT   shut down
  SIGHUP            reopen the log file after rotation
`)
}
