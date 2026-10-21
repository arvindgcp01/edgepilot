// Package config loads and validates EdgePilot server configuration from the
// environment.
//
// The contract: Load either returns a fully valid Config or an error listing
// every problem it found. Nothing downstream re-validates, and nothing has to
// cope with a half-configured server. A misconfiguration is a failed deploy —
// visible immediately, in one place — rather than a nil dereference at first
// use or a security control that silently did not engage.
//
// Key material is parsed here too, not merely copied. A malformed Ed25519 key
// fails at boot with a clear message instead of at the first login attempt.
package config

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Environment selects the strictness of validation and a handful of defaults.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

func (e Environment) IsProduction() bool { return e == EnvProduction }

// StorageDriver selects the object-store backend.
type StorageDriver string

const (
	StorageAzureBlob  StorageDriver = "azblob"
	StorageFilesystem StorageDriver = "fs"
)

// Config is the validated runtime configuration.
type Config struct {
	Env        Environment
	ReplicaID  string
	PublicHost string

	HTTP      HTTPConfig
	GRPC      GRPCConfig
	SSH       SSHConfig
	Admin     AdminConfig
	Database  DatabaseConfig
	Redis     RedisConfig
	Auth      AuthConfig
	CA        CAConfig
	Signing   SigningConfig
	Storage   StorageConfig
	Agent     AgentPolicyConfig
	Telemetry TelemetryConfig
	Log       LogConfig
}

type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// Origins permitted by CORS. Empty means same-origin only, which is correct
	// when the dashboard is served from the same host.
	AllowedOrigins []string
	// MaxRequestBytes caps ordinary JSON bodies. Large artifacts never come
	// through the API — they go straight to object storage via a signed URL —
	// so this can stay small.
	MaxRequestBytes int64
}

type GRPCConfig struct {
	Addr string
	// Interval and timeout for HTTP/2 keepalive on agent streams. A Mac that
	// drops off the network without closing its socket must be noticed, not
	// left as a phantom connection holding presence.
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	MaxRecvBytes      int
	// TLS material for the device plane. Distinct from the public API
	// certificate: this listener requires client certificates.
	CertFile string
	KeyFile  string
}

type SSHConfig struct {
	Addr string
	// ClientKeyFile is the private key the *browser terminal* logs into devices
	// with. Empty disables the browser terminal; `ssh -J` is unaffected.
	//
	// This is a genuinely different trust decision from HostKeyFile. The host
	// key proves the bastion's identity to an operator. This one is a credential
	// that opens a shell on every Mac the matching public key is installed on,
	// which is the price of terminating SSH server-side so a browser can render
	// it. See docs/Security.md §5.1.
	ClientKeyFile string

	// RecordingDir is where browser-terminal sessions are written as asciicast
	// files. Empty disables recording.
	RecordingDir string

	// HostKeyFile is the bastion's SSH host key. Its fingerprint is handed to
	// operators so they can pin it rather than trust on first use.
	HostKeyFile string
	// PublicHost and PublicPort are what appears in the generated `ssh -J`
	// command; behind a load balancer they differ from Addr.
	PublicHost  string
	PublicPort  int
	IdleTimeout time.Duration
	MaxSessions int
}

type AdminConfig struct {
	// Addr serves /healthz, /readyz and /metrics. Bound separately from the
	// public API so it can be kept off the internet by network policy rather
	// than by remembering to add an auth check.
	Addr string
}

type DatabaseConfig struct {
	URL             Secret
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	// StatementTimeout bounds any single query. Without it one pathological
	// query can hold a connection until the pool is exhausted.
	StatementTimeout time.Duration
}

type RedisConfig struct {
	URL          Secret
	PoolSize     int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

type AuthConfig struct {
	// JWTPrivateKey signs access tokens. Parsed at load, so a malformed key is
	// a failed boot rather than a failed login.
	JWTPrivateKey ed25519.PrivateKey
	JWTPublicKey  ed25519.PublicKey

	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	// DataKey seals TOTP secrets at rest (AES-256-GCM), so a database
	// disclosure does not yield working second factors.
	DataKey []byte

	// Argon2id parameters. Defaults follow the RFC 9106 second recommendation.
	ArgonMemoryKiB  uint32
	ArgonIterations uint32
	ArgonParallel   uint8

	MaxFailedLogins int
	LockoutDuration time.Duration
}

type CAConfig struct {
	CertFile string
	KeyFile  string
	// DeviceCertTTL is how long an issued device certificate is valid. Short,
	// because renewal is automatic over the authenticated stream.
	DeviceCertTTL time.Duration
	// RenewBefore is how far ahead of expiry an agent is told to renew.
	RenewBefore time.Duration
}

type SigningConfig struct {
	// PublicKeys are the Ed25519 keys agents pin for artifact, release, and
	// custom-script signatures. A list, because rotation is additive: publish
	// the new key, wait for the fleet to converge, then withdraw the old one.
	PublicKeys []ed25519.PublicKey
	// PrivateKey is present only where this process signs. It is normally
	// absent: signing belongs to a separate service holding an HSM-backed key,
	// so that owning the API server is not sufficient to sign a payload.
	PrivateKey ed25519.PrivateKey
}

type StorageConfig struct {
	Driver StorageDriver

	AccountName   string
	ContainerName string
	AccountKey    Secret // absent when using workload identity, which is preferred

	// FilesystemRoot backs the `fs` driver used in development and single-node
	// deployments.
	FilesystemRoot string

	// SignedURLTTL bounds how long an upload or download URL stays usable.
	SignedURLTTL time.Duration
}

// AgentPolicyConfig is pushed to agents in ConfigUpdate.
type AgentPolicyConfig struct {
	HeartbeatInterval time.Duration
	MetricsInterval   time.Duration
	InventoryInterval time.Duration
	// OfflineAfter is how long without a heartbeat before a device is marked
	// offline. Must exceed HeartbeatInterval by enough to absorb one miss.
	OfflineAfter time.Duration
	// DevicePlaneEndpoint is the host:port agents are told to connect to.
	DevicePlaneEndpoint   string
	MaxConcurrentCommands int
	SpoolMaxBytes         int64
}

type TelemetryConfig struct {
	OTLPEndpoint   string
	ServiceName    string
	TraceRatio     float64
	MetricsEnabled bool
}

type LogConfig struct {
	Level  string
	Format string // json | text
}

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom reads configuration from an arbitrary source, so tests need not
// mutate global process state.
func LoadFrom(src func(string) (string, bool)) (*Config, error) {
	l := &loader{src: src}

	env := Environment(l.enum("EP_ENV", string(EnvDevelopment),
		string(EnvDevelopment), string(EnvStaging), string(EnvProduction)))
	l.prod = env.IsProduction()

	cfg := &Config{
		Env: env,
		// Defaults to the hostname. Must be unique per replica: it is the key
		// the presence map uses to route a command to whichever replica owns
		// the device's stream.
		ReplicaID:  l.str("EP_REPLICA_ID", defaultReplicaID()),
		PublicHost: l.requiredInProd("EP_PUBLIC_HOST", "localhost"),
	}

	cfg.HTTP = HTTPConfig{
		Addr:            l.addr("EP_HTTP_ADDR", ":8080"),
		ReadTimeout:     l.dur("EP_HTTP_READ_TIMEOUT", 15*time.Second, time.Second, 5*time.Minute),
		WriteTimeout:    l.dur("EP_HTTP_WRITE_TIMEOUT", 30*time.Second, time.Second, 5*time.Minute),
		IdleTimeout:     l.dur("EP_HTTP_IDLE_TIMEOUT", 120*time.Second, time.Second, time.Hour),
		ShutdownTimeout: l.dur("EP_HTTP_SHUTDOWN_TIMEOUT", 30*time.Second, time.Second, 5*time.Minute),
		AllowedOrigins:  l.list("EP_HTTP_ALLOWED_ORIGINS", nil),
		MaxRequestBytes: int64(l.intv("EP_HTTP_MAX_REQUEST_BYTES", 1<<20, 1024, 64<<20)),
	}

	cfg.GRPC = GRPCConfig{
		Addr:              l.addr("EP_GRPC_ADDR", ":9443"),
		KeepaliveInterval: l.dur("EP_GRPC_KEEPALIVE_INTERVAL", 30*time.Second, 5*time.Second, 10*time.Minute),
		KeepaliveTimeout:  l.dur("EP_GRPC_KEEPALIVE_TIMEOUT", 10*time.Second, time.Second, time.Minute),
		MaxRecvBytes:      l.intv("EP_GRPC_MAX_RECV_BYTES", 16<<20, 64<<10, 256<<20),
		CertFile:          l.requiredInProd("EP_GRPC_CERT_FILE", ""),
		KeyFile:           l.requiredInProd("EP_GRPC_KEY_FILE", ""),
	}

	cfg.SSH = SSHConfig{
		Addr:          l.addr("EP_SSH_ADDR", ":2222"),
		HostKeyFile:   l.requiredInProd("EP_SSH_HOST_KEY_FILE", ""),
		PublicHost:    l.str("EP_SSH_PUBLIC_HOST", cfg.PublicHost),
		PublicPort:    l.intv("EP_SSH_PUBLIC_PORT", 2222, 1, 65535),
		IdleTimeout:   l.dur("EP_SSH_IDLE_TIMEOUT", 30*time.Minute, time.Minute, 24*time.Hour),
		MaxSessions:   l.intv("EP_SSH_MAX_SESSIONS", 200, 1, 10000),
		ClientKeyFile: l.str("EP_SSH_CLIENT_KEY_FILE", ""),
		RecordingDir:  l.str("EP_SESSION_RECORDING_DIR", ""),
	}

	cfg.Admin = AdminConfig{Addr: l.addr("EP_ADMIN_ADDR", ":9090")}

	cfg.Database = DatabaseConfig{
		URL:              l.secret("EP_DATABASE_URL", true),
		MaxOpenConns:     l.intv("EP_DATABASE_MAX_CONNS", 25, 1, 500),
		MaxIdleConns:     l.intv("EP_DATABASE_MAX_IDLE_CONNS", 5, 0, 500),
		ConnMaxLifetime:  l.dur("EP_DATABASE_CONN_MAX_LIFETIME", 30*time.Minute, time.Minute, 24*time.Hour),
		ConnMaxIdleTime:  l.dur("EP_DATABASE_CONN_MAX_IDLE_TIME", 5*time.Minute, time.Second, time.Hour),
		StatementTimeout: l.dur("EP_DATABASE_STATEMENT_TIMEOUT", 30*time.Second, time.Second, 10*time.Minute),
	}

	cfg.Redis = RedisConfig{
		URL:          l.secret("EP_REDIS_URL", true),
		PoolSize:     l.intv("EP_REDIS_POOL_SIZE", 20, 1, 500),
		DialTimeout:  l.dur("EP_REDIS_DIAL_TIMEOUT", 5*time.Second, time.Second, time.Minute),
		ReadTimeout:  l.dur("EP_REDIS_READ_TIMEOUT", 3*time.Second, time.Second, time.Minute),
		WriteTimeout: l.dur("EP_REDIS_WRITE_TIMEOUT", 3*time.Second, time.Second, time.Minute),
	}

	cfg.Auth = AuthConfig{
		AccessTokenTTL:  l.dur("EP_ACCESS_TOKEN_TTL", 15*time.Minute, time.Minute, time.Hour),
		RefreshTokenTTL: l.dur("EP_REFRESH_TOKEN_TTL", 30*24*time.Hour, time.Hour, 365*24*time.Hour),
		ArgonMemoryKiB:  uint32(l.intv("EP_ARGON_MEMORY_KIB", 64*1024, 8*1024, 1024*1024)),
		ArgonIterations: uint32(l.intv("EP_ARGON_ITERATIONS", 3, 1, 20)),
		ArgonParallel:   uint8(l.intv("EP_ARGON_PARALLELISM", 4, 1, 64)),
		MaxFailedLogins: l.intv("EP_MAX_FAILED_LOGINS", 5, 1, 100),
		LockoutDuration: l.dur("EP_LOCKOUT_DURATION", 15*time.Minute, time.Minute, 24*time.Hour),
	}
	cfg.Auth.JWTPrivateKey, cfg.Auth.JWTPublicKey = l.ed25519PrivateKey("EP_JWT_PRIVATE_KEY", true)
	cfg.Auth.DataKey = l.aesKey("EP_DATA_KEY", true)

	cfg.CA = CAConfig{
		CertFile:      l.requiredInProd("EP_CA_CERT", ""),
		KeyFile:       l.requiredInProd("EP_CA_KEY", ""),
		DeviceCertTTL: l.dur("EP_DEVICE_CERT_TTL", 90*24*time.Hour, 24*time.Hour, 825*24*time.Hour),
		RenewBefore:   l.dur("EP_DEVICE_CERT_RENEW_BEFORE", 30*24*time.Hour, time.Hour, 400*24*time.Hour),
	}

	cfg.Signing = SigningConfig{
		PublicKeys: l.ed25519PublicKeys("EP_SIGNING_PUBLIC_KEYS", l.prod),
	}
	if priv, _ := l.ed25519PrivateKey("EP_SIGNING_PRIVATE_KEY", false); priv != nil {
		cfg.Signing.PrivateKey = priv
	}

	driver := StorageDriver(l.enum("EP_STORAGE_DRIVER", string(StorageFilesystem),
		string(StorageAzureBlob), string(StorageFilesystem)))
	cfg.Storage = StorageConfig{
		Driver:       driver,
		SignedURLTTL: l.dur("EP_STORAGE_SIGNED_URL_TTL", 15*time.Minute, time.Minute, 24*time.Hour),
	}
	switch driver {
	case StorageAzureBlob:
		cfg.Storage.AccountName = l.required("EP_AZURE_STORAGE_ACCOUNT")
		cfg.Storage.ContainerName = l.required("EP_AZURE_STORAGE_CONTAINER")
		cfg.Storage.AccountKey = l.secret("EP_AZURE_STORAGE_KEY", false)
	case StorageFilesystem:
		cfg.Storage.FilesystemRoot = l.str("EP_STORAGE_FS_ROOT", "/var/lib/edgepilot/objects")
	}

	cfg.Agent = AgentPolicyConfig{
		HeartbeatInterval:     l.dur("EP_AGENT_HEARTBEAT_INTERVAL", 15*time.Second, 5*time.Second, 10*time.Minute),
		MetricsInterval:       l.dur("EP_AGENT_METRICS_INTERVAL", 30*time.Second, 10*time.Second, time.Hour),
		InventoryInterval:     l.dur("EP_AGENT_INVENTORY_INTERVAL", 6*time.Hour, time.Minute, 7*24*time.Hour),
		OfflineAfter:          l.dur("EP_AGENT_OFFLINE_AFTER", 90*time.Second, 30*time.Second, time.Hour),
		DevicePlaneEndpoint:   l.str("EP_DEVICE_PLANE_ENDPOINT", ""),
		MaxConcurrentCommands: l.intv("EP_AGENT_MAX_CONCURRENT_COMMANDS", 4, 1, 64),
		SpoolMaxBytes:         int64(l.intv("EP_AGENT_SPOOL_MAX_BYTES", 64<<20, 1<<20, 1<<30)),
	}

	cfg.Telemetry = TelemetryConfig{
		OTLPEndpoint:   l.str("EP_OTLP_ENDPOINT", ""),
		ServiceName:    l.str("EP_SERVICE_NAME", "edgepilot-server"),
		TraceRatio:     float64(l.intv("EP_TRACE_SAMPLE_PERCENT", 10, 0, 100)) / 100,
		MetricsEnabled: l.boolean("EP_METRICS_ENABLED", true),
	}

	cfg.Log = LogConfig{
		Level:  l.enum("EP_LOG_LEVEL", defaultLogLevel(env), "debug", "info", "warn", "error"),
		Format: l.enum("EP_LOG_FORMAT", defaultLogFormat(env), "json", "text"),
	}

	cfg.applyDerivedDefaults()
	cfg.validate(l)

	if len(l.problems) > 0 {
		return nil, &Error{Problems: l.problems}
	}
	return cfg, nil
}

// applyDerivedDefaults fills in values that sensibly follow from others, so an
// operator sets one variable rather than three that must agree.
func (c *Config) applyDerivedDefaults() {
	if c.Agent.DevicePlaneEndpoint == "" && c.PublicHost != "" {
		_, port, err := splitHostPort(c.GRPC.Addr)
		if err == nil {
			c.Agent.DevicePlaneEndpoint = c.PublicHost + ":" + port
		}
	}
	if c.SSH.PublicHost == "" {
		c.SSH.PublicHost = c.PublicHost
	}
}

// validate holds the cross-field and environment-sensitive rules — the ones a
// per-variable check cannot express.
func (c *Config) validate(l *loader) {
	// A device must miss at least one heartbeat before being declared offline,
	// or a single dropped packet marks a healthy Mac down and pages someone.
	if c.Agent.OfflineAfter <= c.Agent.HeartbeatInterval*2 {
		l.fail("EP_AGENT_OFFLINE_AFTER",
			"must be more than twice EP_AGENT_HEARTBEAT_INTERVAL (%s), so one missed heartbeat does not mark a healthy device offline",
			c.Agent.HeartbeatInterval)
	}

	// Renewal has to start before expiry, with room for a device that is offline
	// for a while.
	if c.CA.RenewBefore >= c.CA.DeviceCertTTL {
		l.fail("EP_DEVICE_CERT_RENEW_BEFORE",
			"must be less than EP_DEVICE_CERT_TTL (%s)", c.CA.DeviceCertTTL)
	}

	if c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		l.fail("EP_DATABASE_MAX_IDLE_CONNS",
			"must not exceed EP_DATABASE_MAX_CONNS (%d)", c.Database.MaxOpenConns)
	}

	// Mostly unreachable given the per-variable ranges above (access tops out
	// where refresh begins), and kept deliberately as a backstop: if either
	// range is ever widened, an access token that outlives its refresh token
	// would make rotation and revocation meaningless.
	if c.Auth.AccessTokenTTL >= c.Auth.RefreshTokenTTL {
		l.fail("EP_ACCESS_TOKEN_TTL", "must be shorter than EP_REFRESH_TOKEN_TTL (%s)",
			c.Auth.RefreshTokenTTL)
	}

	// Listeners must not collide; the failure is otherwise a confusing
	// "address already in use" at startup.
	c.validateDistinctAddrs(l)

	if u := c.Database.URL.Reveal(); u != "" {
		validatePostgresURL(l, u, l.prod)
	}
	if u := c.Redis.URL.Reveal(); u != "" {
		validateRedisURL(l, u, l.prod)
	}

	if !l.prod {
		return
	}

	// Production-only hardening.
	if c.PublicHost == "localhost" || strings.HasPrefix(c.PublicHost, "127.") {
		l.failValue("EP_PUBLIC_HOST", c.PublicHost,
			"must be the externally reachable hostname in production")
	}
	if c.Storage.Driver == StorageFilesystem {
		// Permitted — the documented single-node deployment uses it — but it
		// pins the API server to one host, so make the operator opt in.
		if !l.boolean("EP_ALLOW_FILESYSTEM_STORAGE_IN_PRODUCTION", false) {
			l.fail("EP_STORAGE_DRIVER",
				"filesystem storage in production confines the server to a single host; "+
					"use azblob, or set EP_ALLOW_FILESYSTEM_STORAGE_IN_PRODUCTION=true to accept that")
		}
	}
	if len(c.HTTP.AllowedOrigins) > 0 {
		for _, o := range c.HTTP.AllowedOrigins {
			if o == "*" {
				l.failValue("EP_HTTP_ALLOWED_ORIGINS", o,
					"wildcard origin is not permitted in production")
			}
		}
	}
}

func (c *Config) validateDistinctAddrs(l *loader) {
	type named struct {
		key, addr string
	}
	addrs := []named{
		{"EP_HTTP_ADDR", c.HTTP.Addr},
		{"EP_GRPC_ADDR", c.GRPC.Addr},
		{"EP_SSH_ADDR", c.SSH.Addr},
		{"EP_ADMIN_ADDR", c.Admin.Addr},
	}
	seen := map[string]string{}
	for _, a := range addrs {
		if a.addr == "" {
			continue
		}
		if prev, ok := seen[a.addr]; ok {
			l.failValue(a.key, a.addr, "collides with %s; each listener needs its own port", prev)
			continue
		}
		seen[a.addr] = a.key
	}
}

func validatePostgresURL(l *loader, raw string, prod bool) {
	u, err := url.Parse(raw)
	if err != nil {
		l.failSecret("EP_DATABASE_URL", "is not a valid URL")
		return
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		l.failSecret("EP_DATABASE_URL", "must use the postgres:// or postgresql:// scheme")
		return
	}
	if u.Host == "" {
		l.failSecret("EP_DATABASE_URL", "must include a host")
	}
	if strings.TrimPrefix(u.Path, "/") == "" {
		l.failSecret("EP_DATABASE_URL", "must include a database name")
	}
	if prod {
		// Anything weaker than verify-full accepts a certificate that does not
		// match the host, which defeats the point of using TLS at all.
		if mode := u.Query().Get("sslmode"); mode != "verify-full" {
			l.failSecret("EP_DATABASE_URL",
				"must set sslmode=verify-full in production (got %q)", orNone(mode))
		}
	}
}

func validateRedisURL(l *loader, raw string, prod bool) {
	u, err := url.Parse(raw)
	if err != nil {
		l.failSecret("EP_REDIS_URL", "is not a valid URL")
		return
	}
	switch u.Scheme {
	case "redis":
		if prod {
			l.failSecret("EP_REDIS_URL", "must use rediss:// (TLS) in production")
		}
	case "rediss":
	default:
		l.failSecret("EP_REDIS_URL", "must use the redis:// or rediss:// scheme")
	}
	if u.Host == "" {
		l.failSecret("EP_REDIS_URL", "must include a host")
	}
}

// ed25519PrivateKey parses a PKCS#8 PEM private key, returning it with its
// derived public half. Parsing here means a corrupt key is a boot failure with
// a clear message, not an authentication failure hours later.
func (l *loader) ed25519PrivateKey(key string, required bool) (ed25519.PrivateKey, ed25519.PublicKey) {
	raw, ok := l.raw(key)
	if !ok {
		if required {
			l.failSecret(key, "is required")
		}
		return nil, nil
	}

	pemBytes := []byte(decodeMaybeBase64(raw))
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		l.failSecret(key, "must be a PEM-encoded PKCS#8 private key")
		return nil, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		l.failSecret(key, "is not a valid PKCS#8 private key: %v", err)
		return nil, nil
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		l.failSecret(key, "must be an Ed25519 key (EdDSA); other algorithms are not accepted")
		return nil, nil
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		l.failSecret(key, "could not derive a public key")
		return nil, nil
	}
	return priv, pub
}

// ed25519PublicKeys parses a comma-separated list of PEM public keys. A list,
// because signing-key rotation is additive.
func (l *loader) ed25519PublicKeys(key string, required bool) []ed25519.PublicKey {
	raw, ok := l.raw(key)
	if !ok {
		if required {
			l.fail(key, "is required in production: agents pin these keys to verify signed payloads")
		}
		return nil
	}

	var keys []ed25519.PublicKey
	for i, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		block, _ := pem.Decode([]byte(decodeMaybeBase64(entry)))
		if block == nil {
			l.fail(key, "entry %d is not a PEM-encoded public key", i+1)
			continue
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			l.fail(key, "entry %d is not a valid PKIX public key: %v", i+1, err)
			continue
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			l.fail(key, "entry %d must be an Ed25519 public key", i+1)
			continue
		}
		keys = append(keys, pub)
	}
	if required && len(keys) == 0 {
		l.fail(key, "contained no usable Ed25519 public keys")
	}
	return keys
}

// aesKey parses a 32-byte AES-256 key supplied as hex or base64.
func (l *loader) aesKey(key string, required bool) []byte {
	raw, ok := l.raw(key)
	if !ok {
		if required {
			l.failSecret(key, "is required")
		}
		return nil
	}
	b, err := decodeKeyMaterial(raw)
	if err != nil {
		l.failSecret(key, "must be hex or base64 encoded: %v", err)
		return nil
	}
	if len(b) != 32 {
		l.failSecret(key, "must decode to exactly 32 bytes for AES-256 (got %d)", len(b))
		return nil
	}
	return b
}

func orNone(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port in %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

func defaultReplicaID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "edgepilot-server"
}

func defaultLogLevel(env Environment) string {
	if env == EnvDevelopment {
		return "debug"
	}
	return "info"
}

func defaultLogFormat(env Environment) string {
	if env == EnvDevelopment {
		return "text"
	}
	return "json"
}
