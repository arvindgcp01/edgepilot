package logging_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/visionify/edgepilot/apps/agent/internal/logging"
)

func TestLogsReachTheFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "logs", "agent.log")
	log, w, err := logging.New(logging.Options{Path: path, Level: "info"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = w.Close() }()

	log.Info("agent started", "version", "1.0.0")

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(contents), "agent started") {
		t.Errorf("the log line is missing from %s:\n%s", path, contents)
	}
}

// The pairing that keeps logs findable: newsyslog renames the file and sends
// SIGHUP, and the agent reopens. Without the reopen it writes to the renamed
// file for weeks while an operator tails an empty one.
func TestReopeningAfterRotationWritesToTheNewFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	log, w, err := logging.New(logging.Options{Path: path, Level: "info"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = w.Close() }()

	log.Info("before rotation")

	// What newsyslog does: rename, then signal.
	rotated := filepath.Join(dir, "agent.log.0")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := w.Reopen(); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	log.Info("after rotation")

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the current log: %v", err)
	}
	if !strings.Contains(string(current), "after rotation") {
		t.Errorf("the post-rotation line did not reach %s:\n%s", path, current)
	}
	if strings.Contains(string(current), "before rotation") {
		t.Error("the current log holds a line written before the rotation")
	}

	old, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("read the rotated log: %v", err)
	}
	if !strings.Contains(string(old), "before rotation") {
		t.Error("the rotated file lost the line written before the rotation")
	}
}

func TestTheLevelIsHonoured(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.log")
	log, w, err := logging.New(logging.Options{Path: path, Level: "warn"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = w.Close() }()

	log.Debug("a debug line")
	log.Info("an info line")
	log.Warn("a warning")

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if strings.Contains(text, "a debug line") || strings.Contains(text, "an info line") {
		t.Errorf("lines below the configured level were written:\n%s", text)
	}
	if !strings.Contains(text, "a warning") {
		t.Errorf("the warning is missing:\n%s", text)
	}
}

func TestAnUnknownLevelIsRefused(t *testing.T) {
	t.Parallel()

	if _, _, err := logging.New(logging.Options{Level: "chatty"}); err == nil {
		t.Error("an unknown level should be refused rather than silently defaulted")
	}
}

// A foreground run — an operator debugging over SSH — has no log file and must
// still produce output.
func TestWithoutAPathTheLogGoesToStderr(t *testing.T) {
	t.Parallel()

	log, w, err := logging.New(logging.Options{Level: "info"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if w.Path() != "" {
		t.Errorf("path = %q, want empty", w.Path())
	}
	// Must not panic, and must not need a file.
	log.Info("running in the foreground")

	if err := w.Reopen(); err != nil {
		t.Errorf("reopening a stderr writer should be a no-op, got: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("closing a stderr writer should be a no-op, got: %v", err)
	}
}

func TestJSONFormat(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.log")
	log, w, err := logging.New(logging.Options{Path: path, Level: "info", Format: "json"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer func() { _ = w.Close() }()

	log.Info("agent started", "device_id", "abc")

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(contents)), "{") {
		t.Errorf("expected JSON, got:\n%s", contents)
	}
}

// A restart must not truncate what the previous run wrote.
func TestReopeningAppendsRatherThanTruncating(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.log")

	first, w1, err := logging.New(logging.Options{Path: path, Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	first.Info("first run")
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	second, w2, err := logging.New(logging.Options{Path: path, Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Close() }()
	second.Info("second run")

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "first run") {
		t.Errorf("the previous run's log was truncated:\n%s", contents)
	}
	if !strings.Contains(string(contents), "second run") {
		t.Errorf("the current run's log is missing:\n%s", contents)
	}
}
