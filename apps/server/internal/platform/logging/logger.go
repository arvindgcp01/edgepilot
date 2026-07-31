// Package logging builds the process logger.
//
// Two properties matter more than anything else here:
//
//  1. Secrets do not reach the output. Redaction lives in the handler, not in a
//     code-review rule, so it cannot be bypassed by forgetting. See redact.go.
//
//  2. Every line carries its request, trace, and actor identifiers, taken from
//     the context rather than from the call site. See context.go.
//
// Everything else — level, format, destination — is ordinary configuration.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configures the logger. It intentionally mirrors config.LogConfig
// rather than importing it: this package sits in platform/ and must not depend
// on the server's configuration types, or it cannot be reused by epctl or the
// agent.
type Options struct {
	// Level is one of debug, info, warn, error. Unknown values fall back to
	// info rather than failing: a logger that refuses to start leaves no way to
	// report why.
	Level string
	// Format is json or text. Text is for a human at a terminal; json is for
	// anything that will be queried.
	Format string
	// Output defaults to stderr. Logs go to stderr, not stdout, so a command
	// whose stdout is data (epctl emitting JSON, for instance) stays pipeable.
	Output io.Writer
	// AddSource attaches file:line. Useful in development, costly in a hot
	// path, so it is off unless asked for.
	AddSource bool
	// Base attributes bound to every record, such as service and version.
	Base []slog.Attr
}

// New builds a logger with redaction and context propagation wired in.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{
		Level:       parseLevel(opts.Level),
		AddSource:   opts.AddSource,
		ReplaceAttr: redactAttr,
	}

	var base slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		base = slog.NewTextHandler(out, handlerOpts)
	} else {
		base = slog.NewJSONHandler(out, handlerOpts)
	}

	if len(opts.Base) > 0 {
		base = base.WithAttrs(opts.Base)
	}

	return slog.New(contextHandler{base: base})
}

// SetDefault installs l as the process-wide default, so any library reaching
// for slog.Default() gets a redacting logger rather than an unfiltered one.
func SetDefault(l *slog.Logger) { slog.SetDefault(l) }

// Discard returns a logger that writes nowhere. For tests that exercise code
// paths which log, without the noise.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
