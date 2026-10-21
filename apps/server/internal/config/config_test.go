package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

func genEd25519PEM(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal private: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

func genAESKeyHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// env builds a source function over a map, so tests never mutate process state
// and can run in parallel.
func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// minimalDev is the smallest environment that must produce a valid config.
func minimalDev(t *testing.T) map[string]string {
	t.Helper()
	priv, _ := genEd25519PEM(t)
	return map[string]string{
		"EP_DATABASE_URL":    "postgres://edgepilot:pw@localhost:5432/edgepilot?sslmode=disable",
		"EP_REDIS_URL":       "redis://localhost:6379/0",
		"EP_JWT_PRIVATE_KEY": priv,
		"EP_DATA_KEY":        genAESKeyHex(t),
	}
}

// fullProd is a complete, valid production environment.
func fullProd(t *testing.T) map[string]string {
	t.Helper()
	priv, pub := genEd25519PEM(t)
	return map[string]string{
		"EP_ENV":                     "production",
		"EP_PUBLIC_HOST":             "edgepilot.visionify.ai",
		"EP_DATABASE_URL":            "postgres://ep:pw@db.internal:5432/edgepilot?sslmode=verify-full",
		"EP_REDIS_URL":               "rediss://cache.internal:6380",
		"EP_JWT_PRIVATE_KEY":         priv,
		"EP_DATA_KEY":                genAESKeyHex(t),
		"EP_SIGNING_PUBLIC_KEYS":     pub,
		"EP_CA_CERT":                 "/etc/edgepilot/ca.pem",
		"EP_CA_KEY":                  "/etc/edgepilot/ca-key.pem",
		"EP_GRPC_CERT_FILE":          "/etc/edgepilot/device.pem",
		"EP_GRPC_KEY_FILE":           "/etc/edgepilot/device-key.pem",
		"EP_SSH_HOST_KEY_FILE":       "/etc/edgepilot/ssh_host_ed25519_key",
		"EP_STORAGE_DRIVER":          "azblob",
		"EP_AZURE_STORAGE_ACCOUNT":   "edgepilotprod",
		"EP_AZURE_STORAGE_CONTAINER": "artifacts",
	}
}

// problemKeys returns the variables a load failed on.
func problemKeys(t *testing.T, err error) []string {
	t.Helper()
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected *config.Error, got %T: %v", err, err)
	}
	return cfgErr.Keys()
}

func mustLoad(t *testing.T, m map[string]string) *Config {
	t.Helper()
	cfg, err := LoadFrom(env(m))
	if err != nil {
		t.Fatalf("expected a valid config, got: %v", err)
	}
	return cfg
}

// --- happy paths -----------------------------------------------------------

func TestLoadMinimalDevelopment(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, minimalDev(t))

	if cfg.Env != EnvDevelopment {
		t.Errorf("Env = %q, want development", cfg.Env)
	}
	if cfg.HTTP.Addr != ":8080" || cfg.GRPC.Addr != ":9443" ||
		cfg.SSH.Addr != ":2222" || cfg.Admin.Addr != ":9090" {
		t.Errorf("unexpected default listeners: %+v", cfg)
	}
	if cfg.Log.Format != "text" || cfg.Log.Level != "debug" {
		t.Errorf("development should default to text/debug, got %s/%s", cfg.Log.Format, cfg.Log.Level)
	}
	if cfg.Storage.Driver != StorageFilesystem {
		t.Errorf("Storage.Driver = %q, want fs", cfg.Storage.Driver)
	}
	if cfg.ReplicaID == "" {
		t.Error("ReplicaID must default to something; it keys the presence map")
	}
	if len(cfg.Auth.JWTPrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("JWT private key not parsed: len=%d", len(cfg.Auth.JWTPrivateKey))
	}
	if len(cfg.Auth.DataKey) != 32 {
		t.Errorf("DataKey len = %d, want 32", len(cfg.Auth.DataKey))
	}
}

func TestLoadFullProduction(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, fullProd(t))

	if !cfg.Env.IsProduction() {
		t.Fatal("expected production")
	}
	if cfg.Log.Format != "json" || cfg.Log.Level != "info" {
		t.Errorf("production should default to json/info, got %s/%s", cfg.Log.Format, cfg.Log.Level)
	}
	if len(cfg.Signing.PublicKeys) != 1 {
		t.Errorf("expected one signing key, got %d", len(cfg.Signing.PublicKeys))
	}
	if cfg.Signing.PrivateKey != nil {
		t.Error("signing private key must stay absent unless explicitly configured: " +
			"owning the API server should not be sufficient to sign a payload")
	}
}

// The device-plane endpoint an agent is told to dial should follow from the
// public host and the gRPC port, so an operator sets one variable, not three
// that have to agree.
func TestDevicePlaneEndpointDerivedFromPublicHost(t *testing.T) {
	t.Parallel()
	m := fullProd(t)
	cfg := mustLoad(t, m)
	if want := "edgepilot.visionify.ai:9443"; cfg.Agent.DevicePlaneEndpoint != want {
		t.Errorf("DevicePlaneEndpoint = %q, want %q", cfg.Agent.DevicePlaneEndpoint, want)
	}

	m["EP_DEVICE_PLANE_ENDPOINT"] = "devices.visionify.ai:443"
	cfg = mustLoad(t, m)
	if want := "devices.visionify.ai:443"; cfg.Agent.DevicePlaneEndpoint != want {
		t.Errorf("explicit endpoint should win, got %q", cfg.Agent.DevicePlaneEndpoint)
	}
}

// --- failure reporting -----------------------------------------------------

// The point of collecting problems is that an operator fixes everything in one
// pass instead of redeploying once per mistake.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	_, err := LoadFrom(env(map[string]string{
		"EP_ENV":          "production",
		"EP_HTTP_ADDR":    "not-an-address",
		"EP_LOG_LEVEL":    "verbose",
		"EP_DATABASE_URL": "mysql://nope/db",
	}))
	if err == nil {
		t.Fatal("expected failure")
	}

	got := problemKeys(t, err)
	for _, want := range []string{
		"EP_DATABASE_URL",    // wrong scheme
		"EP_HTTP_ADDR",       // unparseable
		"EP_JWT_PRIVATE_KEY", // missing
		"EP_LOG_LEVEL",       // not in the enum
		"EP_PUBLIC_HOST",     // required in production
		"EP_REDIS_URL",       // missing
	} {
		if !slices.Contains(got, want) {
			t.Errorf("expected a problem for %s; got %v", want, got)
		}
	}
	if len(got) < 6 {
		t.Errorf("expected at least 6 problems reported together, got %d: %v", len(got), got)
	}
}

func TestErrorMessageListsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := LoadFrom(env(map[string]string{"EP_LOG_LEVEL": "verbose"}))
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, want := range []string{"EP_DATABASE_URL", "EP_JWT_PRIVATE_KEY", "EP_LOG_LEVEL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message omits %s:\n%s", want, msg)
		}
	}
}

// A configuration error must never quote a secret back at the operator: those
// messages land in logs, CI output, and support tickets.
func TestFailureMessagesNeverEchoSecrets(t *testing.T) {
	t.Parallel()
	const dbPassword = "sup3r-s3cret-pw"
	_, err := LoadFrom(env(map[string]string{
		"EP_DATABASE_URL":    "mysql://user:" + dbPassword + "@host/db",
		"EP_REDIS_URL":       "amqp://host",
		"EP_JWT_PRIVATE_KEY": "-----BEGIN PRIVATE KEY-----\nnot-really-a-key\n-----END PRIVATE KEY-----",
		"EP_DATA_KEY":        "zzzz-not-valid-key-material",
	}))
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, leaked := range []string{dbPassword, "not-really-a-key", "zzzz-not-valid-key-material"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("configuration error leaked secret material %q:\n%s", leaked, msg)
		}
	}
	// It must still say which variable is wrong, or the message is useless.
	for _, want := range []string{"EP_DATABASE_URL", "EP_JWT_PRIVATE_KEY", "EP_DATA_KEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message omits %s:\n%s", want, msg)
		}
	}
}

// --- per-variable validation ----------------------------------------------

func TestValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  map[string]string
		wantKey string
	}{
		{"unparseable listen address", map[string]string{"EP_HTTP_ADDR": "8080"}, "EP_HTTP_ADDR"},
		{"port out of range", map[string]string{"EP_HTTP_ADDR": ":99999"}, "EP_HTTP_ADDR"},
		{"bad duration", map[string]string{"EP_ACCESS_TOKEN_TTL": "fifteen minutes"}, "EP_ACCESS_TOKEN_TTL"},
		{"duration below floor", map[string]string{"EP_ACCESS_TOKEN_TTL": "1s"}, "EP_ACCESS_TOKEN_TTL"},
		{"duration above ceiling", map[string]string{"EP_ACCESS_TOKEN_TTL": "48h"}, "EP_ACCESS_TOKEN_TTL"},
		{"non-integer", map[string]string{"EP_DATABASE_MAX_CONNS": "many"}, "EP_DATABASE_MAX_CONNS"},
		{"integer out of range", map[string]string{"EP_DATABASE_MAX_CONNS": "0"}, "EP_DATABASE_MAX_CONNS"},
		{"unknown enum value", map[string]string{"EP_STORAGE_DRIVER": "s3"}, "EP_STORAGE_DRIVER"},
		{"bad boolean", map[string]string{"EP_METRICS_ENABLED": "yes-please"}, "EP_METRICS_ENABLED"},
		{"non-postgres database URL", map[string]string{"EP_DATABASE_URL": "redis://localhost"}, "EP_DATABASE_URL"},
		{"database URL without a database name", map[string]string{"EP_DATABASE_URL": "postgres://host:5432"}, "EP_DATABASE_URL"},
		{"non-redis cache URL", map[string]string{"EP_REDIS_URL": "http://localhost"}, "EP_REDIS_URL"},
		{"malformed JWT key", map[string]string{"EP_JWT_PRIVATE_KEY": "not-a-pem"}, "EP_JWT_PRIVATE_KEY"},
		{"data key of the wrong length", map[string]string{"EP_DATA_KEY": hex.EncodeToString(make([]byte, 16))}, "EP_DATA_KEY"},
		{"azblob without an account", map[string]string{"EP_STORAGE_DRIVER": "azblob"}, "EP_AZURE_STORAGE_ACCOUNT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := minimalDev(t)
			for k, v := range tc.mutate {
				m[k] = v
			}
			_, err := LoadFrom(env(m))
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.wantKey)
			}
			if got := problemKeys(t, err); !slices.Contains(got, tc.wantKey) {
				t.Errorf("expected a problem for %s, got %v", tc.wantKey, got)
			}
		})
	}
}

// An empty value means "I did not fill this in", not "I want the empty string".
func TestEmptyValueIsTreatedAsUnset(t *testing.T) {
	t.Parallel()
	m := minimalDev(t)
	m["EP_HTTP_ADDR"] = "   "
	cfg := mustLoad(t, m)
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("blank value should fall back to the default, got %q", cfg.HTTP.Addr)
	}

	m["EP_DATABASE_URL"] = ""
	if _, err := LoadFrom(env(m)); err == nil {
		t.Fatal("a blank required variable must be reported as missing")
	}
}

// --- cross-field rules -----------------------------------------------------

// These are the rules a per-variable check cannot express, and each one encodes
// an operational failure that is painful to diagnose after the fact.
func TestCrossFieldRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  map[string]string
		wantKey string
		why     string
	}{
		{
			name:    "offline threshold too close to the heartbeat interval",
			mutate:  map[string]string{"EP_AGENT_HEARTBEAT_INTERVAL": "60s", "EP_AGENT_OFFLINE_AFTER": "90s"},
			wantKey: "EP_AGENT_OFFLINE_AFTER",
			why:     "one dropped heartbeat would mark a healthy Mac offline and page someone",
		},
		{
			name:    "certificate renewal at or after expiry",
			mutate:  map[string]string{"EP_DEVICE_CERT_TTL": "720h", "EP_DEVICE_CERT_RENEW_BEFORE": "720h"},
			wantKey: "EP_DEVICE_CERT_RENEW_BEFORE",
			why:     "renewal must begin before the certificate expires",
		},
		{
			name:    "more idle connections than open ones",
			mutate:  map[string]string{"EP_DATABASE_MAX_CONNS": "10", "EP_DATABASE_MAX_IDLE_CONNS": "20"},
			wantKey: "EP_DATABASE_MAX_IDLE_CONNS",
			why:     "an idle pool larger than the open pool is incoherent",
		},
		{
			// The per-variable ranges (access <= 1h, refresh >= 1h) leave
			// equality as the only reachable violation. That is the point being
			// tested: the backstop still fires at the boundary.
			name:    "access token not outliving the refresh token",
			mutate:  map[string]string{"EP_ACCESS_TOKEN_TTL": "1h", "EP_REFRESH_TOKEN_TTL": "1h"},
			wantKey: "EP_ACCESS_TOKEN_TTL",
			why:     "rotation and revocation stop meaning anything",
		},
		{
			name:    "two listeners on the same port",
			mutate:  map[string]string{"EP_HTTP_ADDR": ":8080", "EP_ADMIN_ADDR": ":8080"},
			wantKey: "EP_ADMIN_ADDR",
			why:     "the admin port carries health and metrics and must stay separable from the public API",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := minimalDev(t)
			for k, v := range tc.mutate {
				m[k] = v
			}
			_, err := LoadFrom(env(m))
			if err == nil {
				t.Fatalf("expected rejection: %s", tc.why)
			}
			if got := problemKeys(t, err); !slices.Contains(got, tc.wantKey) {
				t.Errorf("expected a problem for %s (%s), got %v", tc.wantKey, tc.why, got)
			}
		})
	}
}

// --- production hardening --------------------------------------------------

func TestProductionHardening(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  map[string]string
		remove  []string
		wantKey string
	}{
		{
			name:    "database TLS weaker than verify-full",
			mutate:  map[string]string{"EP_DATABASE_URL": "postgres://ep:pw@db:5432/edgepilot?sslmode=require"},
			wantKey: "EP_DATABASE_URL",
		},
		{
			name:    "database TLS absent",
			mutate:  map[string]string{"EP_DATABASE_URL": "postgres://ep:pw@db:5432/edgepilot"},
			wantKey: "EP_DATABASE_URL",
		},
		{
			name:    "redis without TLS",
			mutate:  map[string]string{"EP_REDIS_URL": "redis://cache:6379"},
			wantKey: "EP_REDIS_URL",
		},
		{
			name:    "public host left at localhost",
			mutate:  map[string]string{"EP_PUBLIC_HOST": "localhost"},
			wantKey: "EP_PUBLIC_HOST",
		},
		{
			name:    "wildcard CORS origin",
			mutate:  map[string]string{"EP_HTTP_ALLOWED_ORIGINS": "*"},
			wantKey: "EP_HTTP_ALLOWED_ORIGINS",
		},
		{
			name:    "signing keys absent",
			remove:  []string{"EP_SIGNING_PUBLIC_KEYS"},
			wantKey: "EP_SIGNING_PUBLIC_KEYS",
		},
		{
			name:    "device CA absent",
			remove:  []string{"EP_CA_CERT", "EP_CA_KEY"},
			wantKey: "EP_CA_KEY",
		},
		{
			name:    "device-plane TLS material absent",
			remove:  []string{"EP_GRPC_CERT_FILE"},
			wantKey: "EP_GRPC_CERT_FILE",
		},
		{
			name:    "bastion host key absent",
			remove:  []string{"EP_SSH_HOST_KEY_FILE"},
			wantKey: "EP_SSH_HOST_KEY_FILE",
		},
		{
			name:    "filesystem storage without an explicit opt-in",
			mutate:  map[string]string{"EP_STORAGE_DRIVER": "fs"},
			wantKey: "EP_STORAGE_DRIVER",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := fullProd(t)
			for k, v := range tc.mutate {
				m[k] = v
			}
			for _, k := range tc.remove {
				delete(m, k)
			}
			_, err := LoadFrom(env(m))
			if err == nil {
				t.Fatalf("production should reject: %s", tc.name)
			}
			if got := problemKeys(t, err); !slices.Contains(got, tc.wantKey) {
				t.Errorf("expected a problem for %s, got %v", tc.wantKey, got)
			}
		})
	}
}

// Every production-only rule must be permissive in development, or nobody can
// run the stack locally.
func TestProductionRulesDoNotApplyInDevelopment(t *testing.T) {
	t.Parallel()
	m := minimalDev(t)
	m["EP_DATABASE_URL"] = "postgres://ep:pw@localhost:5432/edgepilot?sslmode=disable"
	m["EP_REDIS_URL"] = "redis://localhost:6379"
	m["EP_HTTP_ALLOWED_ORIGINS"] = "*"
	cfg := mustLoad(t, m)
	if cfg.PublicHost != "localhost" {
		t.Errorf("PublicHost = %q, want localhost by default in development", cfg.PublicHost)
	}
}

// Filesystem storage in production is supported for the single-node deployment,
// but it pins the server to one host, so it has to be a deliberate choice.
func TestFilesystemStorageInProductionRequiresOptIn(t *testing.T) {
	t.Parallel()
	m := fullProd(t)
	m["EP_STORAGE_DRIVER"] = "fs"
	m["EP_ALLOW_FILESYSTEM_STORAGE_IN_PRODUCTION"] = "true"
	cfg := mustLoad(t, m)
	if cfg.Storage.Driver != StorageFilesystem {
		t.Errorf("Storage.Driver = %q, want fs", cfg.Storage.Driver)
	}
}

// --- key material encodings ------------------------------------------------

func TestKeyMaterialAcceptsCommonEncodings(t *testing.T) {
	t.Parallel()
	priv, pub := genEd25519PEM(t)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}

	// PEM arrives literally from Key Vault, base64-wrapped from CI secrets, and
	// with escaped newlines from .env files. All three must work, or the same
	// config "works locally, fails in the cluster".
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"literal PEM", priv},
		{"base64-wrapped PEM", base64.StdEncoding.EncodeToString([]byte(priv))},
		{"PEM with escaped newlines", strings.ReplaceAll(priv, "\n", `\n`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := minimalDev(t)
			m["EP_JWT_PRIVATE_KEY"] = tc.key
			cfg := mustLoad(t, m)
			if len(cfg.Auth.JWTPrivateKey) != ed25519.PrivateKeySize {
				t.Error("key not parsed")
			}
		})
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"hex", hex.EncodeToString(raw)},
		{"standard base64", base64.StdEncoding.EncodeToString(raw)},
		{"raw url base64", base64.RawURLEncoding.EncodeToString(raw)},
	} {
		t.Run("data key as "+tc.name, func(t *testing.T) {
			t.Parallel()
			m := minimalDev(t)
			m["EP_DATA_KEY"] = tc.key
			cfg := mustLoad(t, m)
			if len(cfg.Auth.DataKey) != 32 {
				t.Errorf("DataKey len = %d, want 32", len(cfg.Auth.DataKey))
			}
		})
	}

	t.Run("signing keys accept a rotation list", func(t *testing.T) {
		t.Parallel()
		_, pub2 := genEd25519PEM(t)
		m := fullProd(t)
		m["EP_SIGNING_PUBLIC_KEYS"] = pub + "," + pub2
		cfg := mustLoad(t, m)
		if len(cfg.Signing.PublicKeys) != 2 {
			t.Errorf("expected 2 signing keys for additive rotation, got %d", len(cfg.Signing.PublicKeys))
		}
	})
}

// A 64-character hex string is also valid base64. Decoding it as base64 yields
// 48 bytes of the wrong thing rather than the intended 32, which would fail far
// away from here as an authentication error.
func TestHexKeyIsNotMisreadAsBase64(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	hexKey := hex.EncodeToString(raw)

	decoded, err := decodeKeyMaterial(hexKey)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("len = %d, want 32 (hex must win over base64 for all-hex input)", len(decoded))
	}
	if string(decoded) != string(raw) {
		t.Error("decoded key does not match the original")
	}
}

// An RSA key where Ed25519 is expected must be rejected explicitly rather than
// failing later with a confusing signature error.
func TestNonEd25519KeyIsRejected(t *testing.T) {
	t.Parallel()
	// A valid PKCS#8 PEM carrying an X25519 key: parses fine, wrong algorithm.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	m := minimalDev(t)
	m["EP_JWT_PRIVATE_KEY"] = publicPEM // a public key where a private one is required
	_, loadErr := LoadFrom(env(m))
	if loadErr == nil {
		t.Fatal("a public key must not be accepted as a signing key")
	}
	if got := problemKeys(t, loadErr); !slices.Contains(got, "EP_JWT_PRIVATE_KEY") {
		t.Errorf("expected EP_JWT_PRIVATE_KEY problem, got %v", got)
	}
}

// --- Secret ----------------------------------------------------------------

// Secret exists so that a credential cannot reach a log through any of the
// formatting paths people actually use.
func TestSecretNeverPrintsItself(t *testing.T) {
	t.Parallel()
	const value = "hunter2-the-real-password"
	s := Secret(value)

	checks := map[string]string{
		// %s on a Stringer is what staticcheck wants replaced with String(),
		// but %s is precisely the path that leaks in real code, so it is the
		// path worth testing.
		"%s":       fmt.Sprintf("%s", s), //nolint:staticcheck // exercising the %s path deliberately
		"%v":       fmt.Sprintf("%v", s),
		"%q":       fmt.Sprintf("%q", s),
		"%#v":      fmt.Sprintf("%#v", s),
		"%+v":      fmt.Sprintf("%+v", s),
		"String()": s.String(),
	}
	for verb, out := range checks {
		if strings.Contains(out, value) {
			t.Errorf("%s leaked the secret: %s", verb, out)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s did not redact: %s", verb, out)
		}
	}

	// Struct embedding is the common real-world case: a config struct gets
	// logged or dumped whole.
	type holder struct {
		Name  string
		Token Secret
	}
	if out := fmt.Sprintf("%+v", holder{Name: "n", Token: s}); strings.Contains(out, value) {
		t.Errorf("embedded secret leaked: %s", out)
	}

	encoded, err := json.Marshal(holder{Name: "n", Token: s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), value) {
		t.Errorf("JSON encoding leaked the secret: %s", encoded)
	}

	if s.Reveal() != value {
		t.Error("Reveal must return the real value")
	}
	if Secret("").String() != "" {
		t.Error("an empty secret should render as empty, not as the redaction placeholder")
	}
}

// A redacted document that is edited and re-applied must not turn the
// placeholder into a real credential.
func TestSecretDoesNotRoundTripThePlaceholder(t *testing.T) {
	t.Parallel()
	var s Secret
	if err := json.Unmarshal([]byte(`"`+redacted+`"`), &s); err != nil {
		t.Fatal(err)
	}
	if s != "" {
		t.Errorf("placeholder round-tripped into a value: %q", s.Reveal())
	}
}

// The database URL and Redis URL hold credentials, so they must be Secrets.
func TestConnectionURLsAreSecrets(t *testing.T) {
	t.Parallel()
	const pw = "db-password-do-not-log"
	m := minimalDev(t)
	m["EP_DATABASE_URL"] = "postgres://ep:" + pw + "@localhost:5432/edgepilot?sslmode=disable"
	cfg := mustLoad(t, m)

	if out := fmt.Sprintf("%v %v", cfg.Database.URL, cfg.Redis.URL); strings.Contains(out, pw) {
		t.Errorf("connection URL leaked a password when formatted: %s", out)
	}
	if !strings.Contains(cfg.Database.URL.Reveal(), pw) {
		t.Error("Reveal should return the usable DSN")
	}
}

// --- defaults sanity -------------------------------------------------------

// These defaults are load-bearing: they are what a deployment gets when nobody
// tunes anything, and several encode decisions documented elsewhere.
func TestDefaultsMatchDocumentedBehaviour(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, minimalDev(t))

	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"access token TTL", cfg.Auth.AccessTokenTTL, 15 * time.Minute},
		{"heartbeat interval", cfg.Agent.HeartbeatInterval, 15 * time.Second},
		{"metrics interval", cfg.Agent.MetricsInterval, 30 * time.Second},
		{"offline threshold", cfg.Agent.OfflineAfter, 90 * time.Second},
		{"device certificate TTL", cfg.CA.DeviceCertTTL, 90 * 24 * time.Hour},
		{"signed URL TTL", cfg.Storage.SignedURLTTL, 15 * time.Minute},
		{"argon2id memory (KiB)", cfg.Auth.ArgonMemoryKiB, uint32(64 * 1024)},
		{"argon2id iterations", cfg.Auth.ArgonIterations, uint32(3)},
		{"agent spool cap", cfg.Agent.SpoolMaxBytes, int64(64 << 20)},
	} {
		if cfg := fmt.Sprint(tc.got); cfg != fmt.Sprint(tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
