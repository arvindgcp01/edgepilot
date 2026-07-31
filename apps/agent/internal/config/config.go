// Package config loads the agent's settings.
//
// Environment variables, read once at startup and validated before anything
// else runs. A misconfigured agent fails at launch with every problem listed,
// rather than at first use with one — on a Mac at a customer site, the
// difference is one visit versus several.
//
// Almost nothing here is required. The agent's real configuration comes from
// the server as policy, and the enrollment response supplies the device-plane
// endpoint. What is left is what the agent needs *before* it can ask: where the
// server is, and the token that lets it introduce itself.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults for a macOS install. Every path is under a root-owned directory,
// because the agent runs as root and its state must not be writable by a local
// user who could then impersonate the device.
const (
	DefaultStateDir  = "/var/db/edgepilot"
	DefaultSpoolDir  = "/var/db/edgepilot/spool"
	DefaultTokenFile = "/etc/edgepilot/enroll"
	DefaultLogFile   = "/var/log/edgepilot/agent.log"
)

// Config is the agent's startup configuration.
type Config struct {
	// ServerURL is the management server's base URL, used only for enrollment.
	// Everything afterwards goes to the device-plane endpoint the server
	// returned.
	ServerURL string

	// Endpoint overrides the device-plane address from enrollment. Set only
	// when a deployment moves its device plane without re-enrolling the fleet.
	Endpoint string

	// TokenFile holds the enrollment token an installer left behind. Read once,
	// and removed after a successful enrollment.
	TokenFile string
	// Token is the token itself, for installs that inject it directly rather
	// than through a file.
	Token string

	StateDir string
	SpoolDir string
	LogFile  string
	LogLevel string

	// ServerCAFile pins the CA that signed the server's *enrollment* TLS
	// certificate. Required for a self-hosted deployment with a private CA;
	// empty means the system roots.
	ServerCAFile string

	// SpoolMaxBytes caps the on-disk buffer. The server can lower it by policy;
	// this is the value used before the first Welcome arrives.
	SpoolMaxBytes int64

	// EnrollRetryMax bounds the wait between enrollment attempts.
	EnrollRetryMax time.Duration
}

// Load reads the configuration from the environment.
//
// Every problem is reported at once. Fixing a bad install should be one pass,
// not one restart per mistake.
func Load(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := &Config{
		ServerURL:      strings.TrimSpace(getenv("EP_SERVER_URL")),
		Endpoint:       strings.TrimSpace(getenv("EP_DEVICE_PLANE_ENDPOINT")),
		TokenFile:      orDefault(getenv("EP_ENROLL_TOKEN_FILE"), DefaultTokenFile),
		Token:          strings.TrimSpace(getenv("EP_ENROLL_TOKEN")),
		StateDir:       orDefault(getenv("EP_STATE_DIR"), DefaultStateDir),
		SpoolDir:       strings.TrimSpace(getenv("EP_SPOOL_DIR")),
		LogFile:        orDefault(getenv("EP_LOG_FILE"), DefaultLogFile),
		LogLevel:       orDefault(getenv("EP_LOG_LEVEL"), "info"),
		ServerCAFile:   strings.TrimSpace(getenv("EP_SERVER_CA_FILE")),
		EnrollRetryMax: 5 * time.Minute,
	}

	// The spool lives under the state directory unless told otherwise, so
	// moving the state directory moves everything with it.
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = filepath.Join(cfg.StateDir, "spool")
	}

	var problems []error

	if raw := strings.TrimSpace(getenv("EP_SPOOL_MAX_BYTES")); raw != "" {
		size, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("EP_SPOOL_MAX_BYTES: %q is not a number", raw))
		case size < 1<<20:
			problems = append(problems, fmt.Errorf(
				"EP_SPOOL_MAX_BYTES: %d is below the 1 MiB minimum; a spool "+
					"smaller than that discards command results faster than they "+
					"can be delivered", size))
		default:
			cfg.SpoolMaxBytes = size
		}
	}

	if raw := strings.TrimSpace(getenv("EP_ENROLL_RETRY_MAX")); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("EP_ENROLL_RETRY_MAX: %q is not a duration", raw))
		case d < time.Second:
			problems = append(problems, fmt.Errorf("EP_ENROLL_RETRY_MAX: %s is too short", d))
		default:
			cfg.EnrollRetryMax = d
		}
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Errorf(
			"EP_LOG_LEVEL: %q is not one of debug, info, warn, error", cfg.LogLevel))
	}

	if cfg.ServerURL != "" {
		if !strings.HasPrefix(cfg.ServerURL, "http://") && !strings.HasPrefix(cfg.ServerURL, "https://") {
			problems = append(problems, fmt.Errorf(
				"EP_SERVER_URL: %q must start with http:// or https://", cfg.ServerURL))
		}
	}

	if cfg.ServerCAFile != "" {
		if _, err := os.Stat(cfg.ServerCAFile); err != nil {
			problems = append(problems, fmt.Errorf(
				"EP_SERVER_CA_FILE: %s cannot be read: %w", cfg.ServerCAFile, err))
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("agent configuration is invalid:\n  %w",
			errors.Join(problems...))
	}
	return cfg, nil
}

// RequireEnrollment reports what is missing for a first-boot enrollment.
//
// Separate from Load because an already-enrolled agent needs none of it: the
// token is gone, the server URL is irrelevant, and refusing to start without
// them would strand a working fleet the first time an installer stopped setting
// them.
func (c *Config) RequireEnrollment() error {
	var problems []error
	if c.ServerURL == "" {
		problems = append(problems, errors.New(
			"EP_SERVER_URL is not set, and this device is not enrolled yet"))
	}
	if c.Token == "" && c.TokenFile == "" {
		problems = append(problems, errors.New(
			"no enrollment token: set EP_ENROLL_TOKEN or EP_ENROLL_TOKEN_FILE"))
	}
	if len(problems) > 0 {
		return fmt.Errorf("this device cannot enroll:\n  %w", errors.Join(problems...))
	}
	return nil
}

func orDefault(value, fallback string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return fallback
}
