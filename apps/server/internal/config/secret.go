package config

import (
	"encoding/json"
	"log/slog"
)

// Secret is a string that will not print itself.
//
// Every accidental credential disclosure starts the same way: something holding
// a secret is passed to a formatter that calls String(), or marshalled into a
// log line, an error, or a debug dump. Secret makes that impossible by
// construction rather than by remembering — fmt, encoding/json, and slog all go
// through one of the methods below, and all of them return the redaction.
//
// Reading the real value requires Reveal(), which makes every legitimate use
// site greppable:
//
//	grep -rn '\.Reveal()' apps/server
//
// should return a short list, and every entry on it should be obviously correct.
//
// This is defence in depth alongside the slog redactor in internal/platform/logging:
// the redactor catches secrets by field name, Secret catches them by type. A
// secret has to slip past both to reach a log.
type Secret string

const redacted = "[REDACTED]"

// Reveal returns the underlying value. Call sites should be few and obvious.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is unset, without disclosing it.
func (s Secret) IsZero() bool { return s == "" }

// String satisfies fmt.Stringer, so %s, %v and Print all redact.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

// GoString satisfies fmt.GoStringer, so %#v redacts too. Without this, the one
// verb people reach for while debugging is the one that leaks.
func (s Secret) GoString() string {
	if s == "" {
		return `""`
	}
	return `"` + redacted + `"`
}

// MarshalJSON ensures a secret never survives serialisation.
func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return json.Marshal(redacted)
}

// MarshalText covers encoders that prefer TextMarshaler, and %s on some paths.
func (s Secret) MarshalText() ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}
	return []byte(redacted), nil
}

// LogValue satisfies slog.LogValuer, so a Secret logged directly as an attribute
// value redacts even when the field name is not one the redactor knows about.
func (s Secret) LogValue() slog.Value {
	if s == "" {
		return slog.StringValue("")
	}
	return slog.StringValue(redacted)
}

// UnmarshalJSON accepts a secret from a config file or API payload. It refuses
// to round-trip the redaction placeholder back into a real value, which would
// otherwise happen when a redacted document is edited and re-applied.
func (s *Secret) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if v == redacted {
		*s = ""
		return nil
	}
	*s = Secret(v)
	return nil
}
