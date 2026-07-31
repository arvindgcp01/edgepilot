// Package logging configures the agent's logger.
//
// # Where the logs go, and why not through a rotation library
//
// The agent writes to a file that launchd points it at, and macOS rotates it
// with newsyslog — the same mechanism that rotates every other system log on the
// machine. That is deliberate: a rotation library inside the agent would hold
// its own file handle, invent its own retention policy, and disagree with the
// one an administrator configured for everything else on the Mac.
//
// What the agent must do is cooperate with newsyslog: reopen its file on SIGHUP,
// because after a rotation the handle it holds points at a file that has been
// renamed. An agent that does not reopen writes to a rotated file for weeks and
// then wonders where its logs went.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Options configures the logger.
type Options struct {
	// Path is the log file. Empty means stderr, which is what a foreground run
	// wants.
	Path string
	// Level is debug, info, warn, or error.
	Level string
	// Format is text or json. Text by default: these logs are read by a person
	// over SSH more often than they are shipped anywhere.
	Format string
}

// Writer is a log destination that can be reopened after rotation.
type Writer struct {
	mu   sync.Mutex
	path string
	file *os.File
}

// New builds the logger and the writer behind it.
//
// The writer is returned so the process can reopen it on SIGHUP; a logger alone
// gives no way to do that.
func New(opts Options) (*slog.Logger, *Writer, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}

	w := &Writer{path: opts.Path}
	if opts.Path != "" {
		if err := w.open(); err != nil {
			return nil, nil, err
		}
	}

	handlerOpts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(opts.Format) {
	case "json":
		handler = slog.NewJSONHandler(w, handlerOpts)
	default:
		handler = slog.NewTextHandler(w, handlerOpts)
	}

	return slog.New(handler), w, nil
}

// Write implements io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return os.Stderr.Write(p)
	}
	return w.file.Write(p)
}

// Reopen closes and reopens the log file.
//
// Called on SIGHUP, which is what newsyslog sends after rotating. Without it the
// agent keeps writing to the renamed file and its logs vanish from the path an
// operator is tailing.
func (w *Writer) Reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path == "" {
		return nil
	}
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("logging: close %s: %w", w.path, err)
		}
		w.file = nil
	}
	return w.openLocked()
}

// Close releases the log file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("logging: close %s: %w", w.path, err)
	}
	return nil
}

func (w *Writer) open() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.openLocked()
}

func (w *Writer) openLocked() error {
	if dir := filepath.Dir(w.path); dir != "." {
		// 0755: an operator needs to read these without sudo, and nothing
		// secret is written to them — the redaction rules keep credentials out.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("logging: create %s: %w", dir, err)
		}
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: open %s: %w", w.path, err)
	}
	w.file = file
	return nil
}

// Path returns the file being written to, or an empty string for stderr.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: %q is not one of debug, info, warn, error", level)
	}
}
