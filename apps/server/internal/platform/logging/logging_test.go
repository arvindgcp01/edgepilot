package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// capture returns a logger writing JSON into a buffer, plus a helper that
// decodes the single record written.
func capture(t *testing.T) (*slog.Logger, func() map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	l := New(Options{Level: "debug", Format: "json", Output: &buf})
	return l, func() map[string]any {
		t.Helper()
		line := strings.TrimSpace(buf.String())
		if line == "" {
			t.Fatal("nothing was logged")
		}
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not valid JSON (%v): %s", err, line)
		}
		return m
	}
}

// raw returns the logger together with the unparsed buffer, for tests that care
// about the bytes rather than the structure.
func rawCapture(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return New(Options{Level: "debug", Format: "json", Output: &buf}), &buf
}

// --- redaction by name -----------------------------------------------------

func TestRedactsSensitiveAttributeNames(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value-that-must-not-appear"

	names := []string{
		"password", "Password", "user_password", "userPassword",
		"passwd", "passphrase",
		"secret", "client_secret", "mfa_secret",
		"token", "access_token", "refresh_token", "Refresh-Token", "enrollment_token",
		"api_key", "apiKey",
		"private_key", "privateKey",
		"authorization", "Authorization", "cookie", "Set-Cookie",
		"csr", "jwt", "otp", "totp", "mfa_code", "recovery_code",
		"data_key", "encryption_key", "signing_key", "account_key",
		"dsn", "connection_string", "database_url", "redis_url",
		"credential", "credentials",
		"upload_url", "download_url", "signed_url", "sas_url",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l, buf := rawCapture(t)
			l.Info("test", slog.String(name, secret))
			if strings.Contains(buf.String(), secret) {
				t.Errorf("attribute %q leaked its value: %s", name, buf.String())
			}
			if !strings.Contains(buf.String(), Redacted) {
				t.Errorf("attribute %q was not redacted: %s", name, buf.String())
			}
		})
	}
}

// Over-redaction is not free: it teaches people to route around the logger.
// These names contain a sensitive word but are safe and genuinely useful.
func TestDoesNotOverRedact(t *testing.T) {
	t.Parallel()
	safe := map[string]string{
		"token_prefix":        "ep_enroll_9f3a",
		"token_count":         "42",
		"token_type":          "Bearer",
		"password_changed_at": "2026-07-31T00:00:00Z",
		"idempotency_key":     "018f2a3b-1111-7222-8333-444455556666",
		"object_key":          "packages/sensor-1.0.0.pkg",
		"cache_key":           "devices:org:123",
		"key_id":              "kid-7",
		"public_key":          "ssh-ed25519 AAAAC3Nza",
		"fingerprint_sha256":  "SHA256:tLd7l9abc",
		"key_type":            "ssh-ed25519",
		"host_key_file":       "/etc/edgepilot/ssh_host_ed25519_key",
		"secret_name":         "edgepilot-jwt",
		"hostname":            "acme-mac-1",
		"user_id":             "cccccccc-0000-0000-0000-000000000001",
	}

	for name, value := range safe {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l, buf := rawCapture(t)
			l.Info("test", slog.String(name, value))
			if strings.Contains(buf.String(), Redacted) {
				t.Errorf("attribute %q was redacted but is safe to log: %s", name, buf.String())
			}
			if !strings.Contains(buf.String(), value) {
				t.Errorf("attribute %q lost its value: %s", name, buf.String())
			}
		})
	}
}

// --- redaction by shape ----------------------------------------------------

// The name-based rule has one blind spot: a credential logged under an
// innocuous key. These are caught by shape instead.
func TestRedactsSecretsUnderInnocuousNames(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"PEM private key":  "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA\n-----END PRIVATE KEY-----",
		"PEM certificate":  "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		"bearer header":    "Bearer eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiIxIn0.sig",
		"basic header":     "Basic dXNlcjpwYXNzd29yZA==",
		"raw JWT":          "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
		"enrollment token": "ep_enroll_9f3a2c1e8b7d6a5f4e3d2c1b0a998877",
		"refresh token":    "ep_refresh_1122334455667788990011223344556677",
		"postgres DSN":     "postgres://edgepilot:hunter2@db.internal:5432/edgepilot",
		"redis DSN":        "rediss://default:cachepass@cache.internal:6380",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Deliberately innocuous attribute names.
			for _, key := range []string{"value", "arg", "line", "detail"} {
				l, buf := rawCapture(t)
				l.Info("test", slog.String(key, value))
				if !strings.Contains(buf.String(), Redacted) {
					t.Errorf("%s under key %q was not redacted: %s", name, key, buf.String())
				}
			}
		})
	}
}

// A URL with no embedded password is the most useful thing in many log lines
// and must survive.
func TestKeepsURLsWithoutCredentials(t *testing.T) {
	t.Parallel()
	for _, url := range []string{
		"https://edgepilot.visionify.ai/api/v1/devices",
		"postgres://db.internal:5432/edgepilot?sslmode=verify-full",
		"rediss://cache.internal:6380",
		"https://example.com/path@with/at",
	} {
		l, buf := rawCapture(t)
		l.Info("test", slog.String("url", url))
		if strings.Contains(buf.String(), Redacted) {
			t.Errorf("URL without credentials was redacted: %s", url)
		}
	}
}

// Byte slices are key material, hashes, or payloads. A hex dump of one is never
// what the author wanted in a log.
func TestRedactsByteSlices(t *testing.T) {
	t.Parallel()
	l, buf := rawCapture(t)
	l.Info("test", slog.Any("payload", []byte{0xde, 0xad, 0xbe, 0xef}))
	if strings.Contains(buf.String(), "deadbeef") || strings.Contains(buf.String(), "3q2+7w") {
		t.Errorf("byte slice was rendered: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("byte slice was not redacted: %s", buf.String())
	}
}

// --- structural coverage ---------------------------------------------------

// Redaction has to survive nesting, or wrapping a payload in a group becomes an
// accidental bypass.
func TestRedactsInsideGroups(t *testing.T) {
	t.Parallel()
	const secret = "nested-secret-value"
	l, buf := rawCapture(t)
	l.Info("test",
		slog.Group("request",
			slog.String("method", "POST"),
			slog.Group("headers", slog.String("authorization", "Bearer "+secret)),
		),
	)
	if strings.Contains(buf.String(), secret) {
		t.Errorf("secret leaked from a nested group: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "POST") {
		t.Errorf("non-sensitive sibling attribute was lost: %s", buf.String())
	}
}

// A whole group logged under a sensitive name collapses rather than leaking its
// members one by one.
func TestCollapsesGroupWithSensitiveName(t *testing.T) {
	t.Parallel()
	l, buf := rawCapture(t)
	l.Info("test", slog.Group("credentials",
		slog.String("user", "admin"),
		slog.String("value", "plaintext-password"),
	))
	if strings.Contains(buf.String(), "plaintext-password") {
		t.Errorf("group under a sensitive name leaked: %s", buf.String())
	}
}

// Attributes bound with With() go through WithAttrs on the handler, a different
// path from per-record attributes.
func TestRedactsAttributesBoundWithWith(t *testing.T) {
	t.Parallel()
	const secret = "bound-secret"
	l, buf := rawCapture(t)
	l.With(slog.String("api_key", secret)).Info("test")
	if strings.Contains(buf.String(), secret) {
		t.Errorf("attribute bound via With() leaked: %s", buf.String())
	}
}

// slog resolves LogValuer before ReplaceAttr, so a type that redacts itself —
// config.Secret is the one that matters — is already safe by the time it
// arrives. This pins that assumption.
func TestResolvesLogValuerBeforeRedacting(t *testing.T) {
	t.Parallel()
	l, buf := rawCapture(t)
	l.Info("test", slog.Any("opaque", selfRedacting{}))
	if strings.Contains(buf.String(), "real-value") {
		t.Errorf("LogValuer was not resolved before output: %s", buf.String())
	}
}

type selfRedacting struct{}

func (selfRedacting) LogValue() slog.Value { return slog.StringValue(Redacted) }
func (selfRedacting) String() string       { return "real-value" }

// An error message frequently carries the input that caused it.
func TestRedactsSecretsInsideErrors(t *testing.T) {
	t.Parallel()
	l, buf := rawCapture(t)
	err := fmt.Errorf("dial failed: %w",
		errors.New("postgres://edgepilot:hunter2@db.internal:5432/edgepilot"))
	l.Error("connect", slog.Any("error", err))
	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("error message leaked a password: %s", buf.String())
	}
}

// --- context propagation ---------------------------------------------------

func TestContextIdentifiersAppearOnEveryLine(t *testing.T) {
	t.Parallel()
	l, decode := capture(t)

	ctx := WithRequestID(context.Background(), "req-123")
	ctx = WithTraceID(ctx, "trace-abc")
	ctx = WithActor(ctx, Actor{
		UserID:   "user-1",
		OrgID:    "org-1",
		DeviceID: "device-1",
	})

	l.InfoContext(ctx, "something happened")

	rec := decode()
	for key, want := range map[string]string{
		"request_id": "req-123",
		"trace_id":   "trace-abc",
		"user_id":    "user-1",
		"org_id":     "org-1",
		"device_id":  "device-1",
	} {
		if got, _ := rec[key].(string); got != want {
			t.Errorf("%s = %q, want %q (record: %v)", key, got, want, rec)
		}
	}
}

func TestContextIdentifiersAreOmittedWhenAbsent(t *testing.T) {
	t.Parallel()
	l, decode := capture(t)
	l.InfoContext(context.Background(), "no context")

	rec := decode()
	for _, key := range []string{"request_id", "trace_id", "user_id", "org_id", "device_id"} {
		if _, present := rec[key]; present {
			t.Errorf("%s should be omitted when unset, got %v", key, rec[key])
		}
	}
}

// The context decoration must survive With() and WithGroup(), or bound loggers
// silently lose their correlation identifiers.
func TestContextSurvivesWithAndWithGroup(t *testing.T) {
	t.Parallel()
	l, decode := capture(t)
	ctx := WithRequestID(context.Background(), "req-456")

	l.With(slog.String("component", "device")).
		WithGroup("op").
		InfoContext(ctx, "still correlated")

	rec := decode()
	if got, _ := rec["request_id"].(string); got != "req-456" {
		t.Errorf("request_id lost after With/WithGroup: %v", rec)
	}
}

func TestFromContextNeverReturnsNil(t *testing.T) {
	t.Parallel()
	if FromContext(context.Background()) == nil {
		t.Fatal("FromContext must not return nil: a logging call is not a place to panic")
	}
	custom := Discard()
	if got := FromContext(WithLogger(context.Background(), custom)); got != custom {
		t.Error("FromContext should return the attached logger")
	}
	//nolint:staticcheck // deliberately storing a nil logger to prove the fallback
	if FromContext(WithLogger(context.Background(), nil)) == nil {
		t.Error("a nil logger in context must fall back to the default")
	}
}

func TestEmptyIdentifiersDoNotEnterContext(t *testing.T) {
	t.Parallel()
	ctx := WithRequestID(context.Background(), "")
	if RequestIDFromContext(ctx) != "" {
		t.Error("empty request ID should not be stored")
	}
	if got := WithTraceID(context.Background(), ""); TraceIDFromContext(got) != "" {
		t.Error("empty trace ID should not be stored")
	}
}

// --- options ---------------------------------------------------------------

func TestLevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{Level: "warn", Format: "json", Output: &buf})

	l.Debug("debug")
	l.Info("info")
	if buf.Len() != 0 {
		t.Errorf("debug and info should be filtered at warn level: %s", buf.String())
	}
	l.Warn("warn")
	if !strings.Contains(buf.String(), "warn") {
		t.Errorf("warn should be emitted: %s", buf.String())
	}
}

// An unrecognised level must not stop the process: a logger that refuses to
// start leaves no way to report why it refused.
func TestUnknownLevelFallsBackToInfo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{Level: "chatty", Format: "json", Output: &buf})
	l.Debug("should be filtered")
	if buf.Len() != 0 {
		t.Errorf("unknown level should behave as info, got: %s", buf.String())
	}
	l.Info("should appear")
	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("info should be emitted: %s", buf.String())
	}
}

func TestBaseAttributesAppearOnEveryLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{
		Level:  "info",
		Format: "json",
		Output: &buf,
		Base:   []slog.Attr{slog.String("service", "edgepilot-server"), slog.String("version", "1.0.0")},
	})
	l.Info("hello")
	out := buf.String()
	if !strings.Contains(out, "edgepilot-server") || !strings.Contains(out, "1.0.0") {
		t.Errorf("base attributes missing: %s", out)
	}
}

// Base attributes are bound through WithAttrs and must still be redacted.
func TestBaseAttributesAreRedacted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{
		Level:  "info",
		Format: "json",
		Output: &buf,
		Base:   []slog.Attr{slog.String("api_key", "base-secret-value")},
	})
	l.Info("hello")
	if strings.Contains(buf.String(), "base-secret-value") {
		t.Errorf("base attribute leaked a secret: %s", buf.String())
	}
}

func TestTextFormat(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{Level: "info", Format: "text", Output: &buf})
	l.Info("hello", slog.String("password", "leaky"))
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("expected text output, got JSON: %s", out)
	}
	if strings.Contains(out, "leaky") {
		t.Errorf("text handler must redact too: %s", out)
	}
}

// --- key classification ----------------------------------------------------

func TestIsSensitiveKey(t *testing.T) {
	t.Parallel()
	sensitive := []string{
		"password", "PASSWORD", "user_password", "userPassword", "user-password",
		"refresh_token", "refreshToken", "api_key", "authorization", "cookie",
		"mfa_secret", "database_url", "dsn",
	}
	safe := []string{
		"token_prefix", "token_count", "access_token_ttl", "password_changed_at",
		"idempotency_key", "object_key", "key_id", "public_key", "hostname",
		"user_id", "device_id", "status", "",
	}

	for _, k := range sensitive {
		if !IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", k)
		}
	}
	for _, k := range safe {
		if IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", k)
		}
	}
}

// --- performance guard -----------------------------------------------------

// Redaction runs on every attribute of every line, including the metrics ingest
// path. Keep an eye on the cost.
func BenchmarkRedactingLogger(b *testing.B) {
	l := New(Options{Level: "info", Format: "json", Output: discardWriter{}})
	ctx := WithRequestID(context.Background(), "req-bench")
	b.ReportAllocs()
	for b.Loop() {
		l.InfoContext(ctx, "device heartbeat",
			slog.String("device_id", "3f9a1c2e-7b4d-4e8a-9c1f-2a5b6d8e0f11"),
			slog.String("hostname", "acme-mac-1"),
			slog.Int("uptime_seconds", 864000),
		)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
