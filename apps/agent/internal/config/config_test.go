package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/visionify/edgepilot/apps/agent/internal/config"
)

// env builds a getenv function from a map, so a test never touches the process
// environment and can therefore run in parallel.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestAnEmptyEnvironmentProducesAWorkingDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.StateDir != config.DefaultStateDir {
		t.Errorf("state dir = %q, want %q", cfg.StateDir, config.DefaultStateDir)
	}
	if cfg.TokenFile != config.DefaultTokenFile {
		t.Errorf("token file = %q, want %q", cfg.TokenFile, config.DefaultTokenFile)
	}
	if cfg.LogFile != config.DefaultLogFile {
		t.Errorf("log file = %q, want %q", cfg.LogFile, config.DefaultLogFile)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log level = %q, want info", cfg.LogLevel)
	}
}

// Moving the state directory has to move the spool with it, or an operator who
// relocates the agent's state leaves its buffer behind in a directory nothing
// cleans up.
func TestTheSpoolFollowsTheStateDirectory(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"EP_STATE_DIR": "/opt/edgepilot"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := filepath.Join("/opt/edgepilot", "spool"); cfg.SpoolDir != want {
		t.Errorf("spool dir = %q, want %q", cfg.SpoolDir, want)
	}

	// Unless it is set explicitly.
	cfg, err = config.Load(env(map[string]string{
		"EP_STATE_DIR": "/opt/edgepilot", "EP_SPOOL_DIR": "/var/spool/edgepilot",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SpoolDir != "/var/spool/edgepilot" {
		t.Errorf("spool dir = %q, want the explicit value", cfg.SpoolDir)
	}
}

// Fixing a bad install should be one pass, not one restart per mistake — which
// on a Mac at a customer site is the difference between one visit and several.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"EP_LOG_LEVEL":        "chatty",
		"EP_SPOOL_MAX_BYTES":  "not-a-number",
		"EP_SERVER_URL":       "edgepilot.example.com",
		"EP_ENROLL_RETRY_MAX": "soon",
	}))
	if err == nil {
		t.Fatal("this configuration should have been refused")
	}

	for _, want := range []string{
		"EP_LOG_LEVEL", "EP_SPOOL_MAX_BYTES", "EP_SERVER_URL", "EP_ENROLL_RETRY_MAX",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s:\n%v", want, err)
		}
	}
}

// A spool too small to hold a command result discards work faster than it can be
// delivered, which looks like the server losing messages.
func TestASpoolTooSmallToBeUsefulIsRefused(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{"EP_SPOOL_MAX_BYTES": "1024"}))
	if err == nil {
		t.Fatal("a 1 KiB spool should have been refused")
	}
	if !strings.Contains(err.Error(), "minimum") {
		t.Errorf("the error should say what the minimum is:\n%v", err)
	}

	cfg, err := config.Load(env(map[string]string{"EP_SPOOL_MAX_BYTES": "8388608"}))
	if err != nil {
		t.Fatalf("8 MiB should be accepted: %v", err)
	}
	if cfg.SpoolMaxBytes != 8<<20 {
		t.Errorf("spool max = %d, want %d", cfg.SpoolMaxBytes, 8<<20)
	}
}

func TestTheServerURLMustCarryAScheme(t *testing.T) {
	t.Parallel()

	for _, url := range []string{"edgepilot.example.com", "ftp://edgepilot.example.com", "//edgepilot"} {
		if _, err := config.Load(env(map[string]string{"EP_SERVER_URL": url})); err == nil {
			t.Errorf("%q should have been refused", url)
		}
	}
	for _, url := range []string{"https://edgepilot.example.com", "http://localhost:8080"} {
		if _, err := config.Load(env(map[string]string{"EP_SERVER_URL": url})); err != nil {
			t.Errorf("%q should have been accepted: %v", url, err)
		}
	}
}

// A pinned CA that cannot be read would fail at the first enrollment attempt,
// hours after install if the network was down. Failing at startup is better.
func TestAnUnreadableCAFileIsCaughtAtStartup(t *testing.T) {
	t.Parallel()

	_, err := config.Load(env(map[string]string{
		"EP_SERVER_CA_FILE": filepath.Join(t.TempDir(), "absent.pem"),
	}))
	if err == nil {
		t.Fatal("a missing CA file should have been refused")
	}

	present := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(present, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(env(map[string]string{"EP_SERVER_CA_FILE": present})); err != nil {
		t.Errorf("a readable CA file should be accepted: %v", err)
	}
}

func TestTheEnrollRetryCeilingIsConfigurable(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"EP_ENROLL_RETRY_MAX": "90s"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.EnrollRetryMax != 90*time.Second {
		t.Errorf("retry max = %s, want 90s", cfg.EnrollRetryMax)
	}

	if _, err := config.Load(env(map[string]string{"EP_ENROLL_RETRY_MAX": "10ms"})); err == nil {
		t.Error("a sub-second retry ceiling should be refused")
	}
}

// An enrolled agent needs none of the enrollment settings. Refusing to start
// without them would strand a working fleet the first time an installer stopped
// setting them.
func TestEnrollmentSettingsAreOnlyRequiredForEnrollment(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Loading is fine...
	if err := cfg.RequireEnrollment(); err == nil {
		t.Fatal("...but enrolling without a server URL must be refused")
	} else if !strings.Contains(err.Error(), "EP_SERVER_URL") {
		t.Errorf("the error should name the missing setting:\n%v", err)
	}

	cfg, err = config.Load(env(map[string]string{
		"EP_SERVER_URL": "https://edgepilot.example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// The token file defaults, so this is enough to attempt an enrollment.
	if err := cfg.RequireEnrollment(); err != nil {
		t.Errorf("a server URL and the default token file should suffice: %v", err)
	}
}

func TestWhitespaceIsTrimmed(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{
		"EP_SERVER_URL":   "  https://edgepilot.example.com  ",
		"EP_ENROLL_TOKEN": "\tep_enroll_secret\n",
		"EP_STATE_DIR":    " /opt/edgepilot ",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ServerURL != "https://edgepilot.example.com" {
		t.Errorf("server URL = %q", cfg.ServerURL)
	}
	if cfg.Token != "ep_enroll_secret" {
		t.Errorf("token = %q", cfg.Token)
	}
	if cfg.StateDir != "/opt/edgepilot" {
		t.Errorf("state dir = %q", cfg.StateDir)
	}
}
