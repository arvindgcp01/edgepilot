// Package testsupport provides shared fixtures for tests that need real
// infrastructure.
//
// Integration tests run against a real PostgreSQL rather than a mock, because
// the things most worth testing here — row-level security, partition routing,
// check constraints, triggers — exist only in the database. A mock would assert
// that the code called the query it was written to call, which is not the same
// as asserting that the query does what it should.
//
// The database is supplied by the environment rather than started per test:
//
//	EP_TEST_DATABASE_URL   admin DSN, e.g. postgres://edgepilot:edgepilot@localhost:5432/edgepilot
//
// `make dev` provides exactly that, and CI supplies it with a service container.
// When it is absent, tests skip with an explanatory message — unless CI is set,
// where a silently skipped integration suite is worse than a failing one.
package testsupport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/visionify/edgepilot/apps/server/internal/platform/migrate"
	"github.com/visionify/edgepilot/apps/server/migrations"
)

const (
	// EnvDatabaseURL names the admin DSN used to create per-test databases.
	EnvDatabaseURL = "EP_TEST_DATABASE_URL"

	// databasePrefix marks scratch databases so a crashed run leaves something
	// obviously disposable behind rather than something ambiguous.
	databasePrefix = "edgepilot_test_"
)

// DB is a disposable database, dropped when the test finishes.
type DB struct {
	// DSN addresses the scratch database.
	DSN string
	// SQL is an open pool against it.
	SQL *sql.DB
	// Name is the scratch database name.
	Name string
}

// NewDatabase creates an empty scratch database.
//
// Each caller gets its own database rather than a shared one with truncation
// between tests. That is what makes the suite safe to run with t.Parallel():
// two tests inserting into `devices` cannot see each other, and neither can
// corrupt the developer's `make dev` data.
func NewDatabase(t *testing.T) *DB {
	t.Helper()

	adminDSN := requireDSN(t)

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	// database/sql opens connections without limit by default. A parallel suite
	// with a scratch database per test then exhausts PostgreSQL's
	// max_connections, and every test fails with "too many clients" —
	// a harness problem that reads exactly like a product bug.
	capConnections(admin)
	defer func() { _ = admin.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("%s is set but unreachable: %v", EnvDatabaseURL, err)
	}

	name := scratchName(t)

	// The database name is generated here, never taken from input, so
	// interpolating it is safe — and necessary, because CREATE DATABASE does
	// not accept placeholders.
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}

	dsn := replaceDatabase(adminDSN, name)
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		dropDatabase(t, adminDSN, name)
		t.Fatalf("open scratch database: %v", err)
	}
	capConnections(pool)
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		dropDatabase(t, adminDSN, name)
		t.Fatalf("ping scratch database: %v", err)
	}

	t.Cleanup(func() {
		// Close the pool before dropping: PostgreSQL refuses to drop a database
		// that still has a session attached.
		_ = pool.Close()
		dropDatabase(t, adminDSN, name)
	})

	return &DB{DSN: dsn, SQL: pool, Name: name}
}

// NewMigratedDatabase creates a scratch database with the full schema applied.
func NewMigratedDatabase(t *testing.T) *DB {
	t.Helper()

	db := NewDatabase(t)
	if err := Migrate(t.Context(), db.DSN); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

// Migrate applies every migration to the database at dsn.
func Migrate(ctx context.Context, dsn string) error {
	runner, err := migrate.New(migrate.Options{
		DatabaseURL: dsn,
		Source:      migrations.FS,
		Logger:      quietLogger(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = runner.Close() }()
	return runner.Up(ctx)
}

// ScopeToOrg sets the tenant scope for a transaction, exactly as the repository
// layer does. Tests that assert row-level security behaviour must go through
// this rather than querying as the owner, or they prove nothing.
func ScopeToOrg(t *testing.T, tx *sql.Tx, orgID string) {
	t.Helper()
	ctx := t.Context()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE edgepilot_app`); err != nil {
		t.Fatalf("assume application role: %v", err)
	}
	// set_config with is_local=true is used rather than SET LOCAL because the
	// value is a parameter here, and SET LOCAL takes no placeholders.
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('edgepilot.org_id', $1, true)`, orgID); err != nil {
		t.Fatalf("scope to org %s: %v", orgID, err)
	}
}

// --- internals -------------------------------------------------------------

// maxTestConnections is how many connections one test's pool may open.
//
// PostgreSQL's default max_connections is 100. A suite that runs dozens of
// packages in parallel, each with a scratch database, will exhaust that in
// seconds without a cap — and the resulting "sorry, too many clients already"
// looks like a product failure rather than a harness one. Two is enough: a
// test issues one query at a time, with the occasional concurrent case.
const maxTestConnections = 2

// capConnections bounds a pool. database/sql opens connections without limit by
// default, which is the wrong default for a parallel test suite.
func capConnections(db *sql.DB) {
	db.SetMaxOpenConns(maxTestConnections)
	db.SetMaxIdleConns(1)
	// Short, so a pool held open by a slow cleanup releases its connections
	// rather than holding them for the rest of the run.
	db.SetConnMaxIdleTime(10 * time.Second)
}

func requireDSN(t *testing.T) string {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv(EnvDatabaseURL))
	if dsn != "" {
		return dsn
	}

	// In CI a skipped integration suite looks identical to a passing one, which
	// is how coverage quietly disappears. Fail instead.
	if os.Getenv("CI") != "" {
		t.Fatalf("%s is not set. CI must provide PostgreSQL for integration tests.", EnvDatabaseURL)
	}
	t.Skipf("%s is not set; run `make dev` and export it to run integration tests", EnvDatabaseURL)
	return ""
}

// scratchName derives a readable, unique, valid identifier from the test name.
func scratchName(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	b.WriteString(databasePrefix)
	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate database suffix: %v", err)
	}
	name := b.String() + "_" + hex.EncodeToString(suffix)

	// PostgreSQL truncates identifiers at 63 bytes; truncating here keeps the
	// random suffix, which is the part that guarantees uniqueness.
	if len(name) > 63 {
		name = name[:63-13] + "_" + hex.EncodeToString(suffix)
	}
	return name
}

func dropDatabase(t *testing.T, adminDSN, name string) {
	t.Helper()

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Logf("cleanup: open admin connection: %v", err)
		return
	}
	defer func() { _ = admin.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// FORCE terminates any session the test left behind, so a leaked connection
	// does not turn into a leaked database.
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		t.Logf("cleanup: drop database %s: %v (drop it manually if it persists)", name, err)
	}
}

// replaceDatabase swaps the database name in a DSN, preserving credentials and
// query parameters such as sslmode.
func replaceDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// Not a URL-form DSN; fall back to appending, which works for keyword
		// form ("host=... dbname=...") only if dbname is absent. Callers use
		// URL form, so this is a safety net rather than a supported path.
		return dsn + " dbname=" + name
	}
	u.Path = "/" + name
	return u.String()
}

// quietLogger keeps migration chatter out of test output. A failing migration
// still surfaces through the returned error.
func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
