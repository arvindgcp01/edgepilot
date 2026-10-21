package config

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Problem is one thing wrong with the configuration.
type Problem struct {
	Key    string // the environment variable, e.g. EP_DATABASE_URL
	Detail string // what is wrong, in a form an operator can act on
	Value  string // the offending value; always empty for secrets
	secret bool
}

func (p Problem) String() string {
	if p.secret || p.Value == "" {
		return fmt.Sprintf("%s: %s", p.Key, p.Detail)
	}
	return fmt.Sprintf("%s: %s (got %q)", p.Key, p.Detail, p.Value)
}

// Error reports everything wrong with the configuration at once.
//
// Reporting only the first problem turns a misconfigured deployment into a
// guessing game: fix one variable, redeploy, discover the next. Operators get
// the whole list on the first attempt.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0].String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration: %d problems", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.String())
	}
	return b.String()
}

// Keys returns the offending variable names, sorted. Useful in tests and for
// emitting a machine-readable startup failure.
func (e *Error) Keys() []string {
	keys := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		keys = append(keys, p.Key)
	}
	sort.Strings(keys)
	return keys
}

// loader reads and validates environment variables, accumulating problems
// instead of stopping at the first one.
type loader struct {
	src      func(string) (string, bool)
	problems []Problem
	prod     bool // stricter rules apply
}

func (l *loader) fail(key, format string, args ...any) {
	l.problems = append(l.problems, Problem{Key: key, Detail: fmt.Sprintf(format, args...)})
}

func (l *loader) failValue(key, value, format string, args ...any) {
	l.problems = append(l.problems, Problem{Key: key, Detail: fmt.Sprintf(format, args...), Value: value})
}

func (l *loader) failSecret(key, format string, args ...any) {
	l.problems = append(l.problems, Problem{Key: key, Detail: fmt.Sprintf(format, args...), secret: true})
}

// raw returns the trimmed value and whether it was set to something non-empty.
// An empty string is treated as unset: `EP_FOO=` in a compose file almost always
// means "I did not fill this in", not "I want the empty value".
func (l *loader) raw(key string) (string, bool) {
	v, ok := l.src(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	v, ok := l.raw(key)
	if !ok {
		l.fail(key, "is required")
		return ""
	}
	return v
}

// requiredInProd is optional in development (where a sensible default exists)
// and mandatory in production (where guessing is dangerous).
func (l *loader) requiredInProd(key, devDefault string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	if l.prod {
		l.fail(key, "is required in production")
		return ""
	}
	return devDefault
}

func (l *loader) secret(key string, required bool) Secret {
	v, ok := l.raw(key)
	if !ok {
		if required {
			l.failSecret(key, "is required")
		}
		return ""
	}
	return Secret(v)
}

func (l *loader) intv(key string, def, minimum, maximum int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.failValue(key, v, "must be an integer")
		return def
	}
	if n < minimum || n > maximum {
		l.failValue(key, v, "must be between %d and %d", minimum, maximum)
		return def
	}
	return n
}

func (l *loader) dur(key string, def, minimum, maximum time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.failValue(key, v, "must be a duration such as 30s, 15m, or 24h")
		return def
	}
	if d < minimum || d > maximum {
		l.failValue(key, v, "must be between %s and %s", minimum, maximum)
		return def
	}
	return d
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.failValue(key, v, "must be a boolean (true/false, 1/0)")
		return def
	}
	return b
}

func (l *loader) enum(key, def string, allowed ...string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.failValue(key, v, "must be one of: %s", strings.Join(allowed, ", "))
	return def
}

// addr validates a listen address. A bare port is rejected: ":8080" and "8080"
// differ in a way that produces a confusing bind error much later.
func (l *loader) addr(key, def string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		l.failValue(key, v, "must be a listen address such as :8080 or 127.0.0.1:8080")
		return def
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		l.failValue(key, v, "port must be between 1 and 65535")
		return def
	}
	return v
}

// list splits a comma-separated value, dropping empty entries.
func (l *loader) list(key string, def []string) []string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
