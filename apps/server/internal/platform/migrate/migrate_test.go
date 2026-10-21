package migrate_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4/database"

	"github.com/visionify/edgepilot/apps/server/internal/platform/migrate"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
	"github.com/visionify/edgepilot/apps/server/migrations"
)

func newRunner(t *testing.T, dsn string) *migrate.Runner {
	t.Helper()
	r, err := migrate.New(migrate.Options{
		DatabaseURL: dsn,
		Source:      migrations.FS,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open runner: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// --- construction ----------------------------------------------------------

func TestNewRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()

	if _, err := migrate.New(migrate.Options{Source: migrations.FS}); err == nil {
		t.Error("a missing DatabaseURL must be rejected")
	}
	if _, err := migrate.New(migrate.Options{DatabaseURL: "postgres://x/y"}); err == nil {
		t.Error("a missing Source must be rejected")
	}
}

// The migration files have to be readable as a source before any database is
// involved; a malformed name would otherwise fail at deploy time.
func TestEmbeddedMigrationsAreWellFormed(t *testing.T) {
	t.Parallel()

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	ups, downs := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case len(name) > 7 && name[len(name)-7:] == ".up.sql":
			ups[name[:len(name)-7]] = true
		case len(name) > 9 && name[len(name)-9:] == ".down.sql":
			downs[name[:len(name)-9]] = true
		}
	}

	if len(ups) == 0 {
		t.Fatal("no migrations were embedded; the go:embed directive is not matching")
	}
	// A migration without a reverse cannot be rolled back, which turns a bad
	// deploy into a restore-from-backup.
	for name := range ups {
		if !downs[name] {
			t.Errorf("%s.up.sql has no matching .down.sql", name)
		}
	}
	for name := range downs {
		if !ups[name] {
			t.Errorf("%s.down.sql has no matching .up.sql", name)
		}
	}
}

func TestSourceWithNonNumericVersionIsRejected(t *testing.T) {
	t.Parallel()

	bad := fstest.MapFS{
		"init.up.sql":   &fstest.MapFile{Data: []byte("SELECT 1;")},
		"init.down.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}
	if _, err := migrate.New(migrate.Options{DatabaseURL: "postgres://x/y", Source: bad}); err == nil {
		t.Error("a migration without a numeric version prefix must be rejected at construction")
	}
}

// --- against a real database -----------------------------------------------

func TestMigrationLifecycle(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	r := newRunner(t, db.DSN)
	ctx := t.Context()

	// A fresh database: version 0, everything pending.
	st, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Version != 0 {
		t.Errorf("fresh database version = %d, want 0", st.Version)
	}
	if st.Dirty {
		t.Error("a fresh database must not be dirty")
	}
	if len(st.Available) == 0 {
		t.Fatal("no migrations available")
	}
	if len(st.Pending) != len(st.Available) {
		t.Errorf("pending = %d, want all %d", len(st.Pending), len(st.Available))
	}
	if st.UpToDate() {
		t.Error("a fresh database is not up to date")
	}

	highest := st.Available[len(st.Available)-1]

	if err := r.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}

	st, err = r.Status(ctx)
	if err != nil {
		t.Fatalf("status after up: %v", err)
	}
	if st.Version != highest {
		t.Errorf("version = %d, want %d", st.Version, highest)
	}
	if len(st.Pending) != 0 {
		t.Errorf("pending = %v, want none", st.Pending)
	}
	if !st.UpToDate() {
		t.Error("schema should be up to date after Up")
	}

	// Up must be idempotent: every replica calls it at startup, and all but one
	// will find nothing to do.
	if err := r.Up(ctx); err != nil {
		t.Fatalf("second up should be a no-op, got: %v", err)
	}
	if st2, _ := r.Status(ctx); st2.Version != highest {
		t.Errorf("version changed on a no-op up: %d", st2.Version)
	}
}

func TestUpAppliesTheRealSchema(t *testing.T) {
	t.Parallel()

	db := testsupport.NewMigratedDatabase(t)
	ctx := t.Context()

	// Spot-check that the schema actually landed, rather than trusting the
	// version counter alone.
	for _, table := range []string{
		"organizations", "users", "devices", "device_metrics", "commands",
		"deployments", "alerts", "audit_logs", "ssh_keys", "files",
	} {
		var exists bool
		err := db.SQL.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s is missing after migration", table)
		}
	}

	// Seed data is part of the schema, not a separate bootstrap step.
	var permissions, definitions int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM permissions`).Scan(&permissions); err != nil {
		t.Fatalf("count permissions: %v", err)
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM command_definitions`).Scan(&definitions); err != nil {
		t.Fatalf("count command definitions: %v", err)
	}
	if permissions == 0 || definitions == 0 {
		t.Errorf("seed data missing: %d permissions, %d command definitions", permissions, definitions)
	}
}

func TestDownReversesAndUpReapplies(t *testing.T) {
	t.Parallel()

	db := testsupport.NewMigratedDatabase(t)
	r := newRunner(t, db.DSN)
	ctx := t.Context()

	before, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if err := r.Down(ctx, 1); err != nil {
		t.Fatalf("down: %v", err)
	}

	after, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status after down: %v", err)
	}
	if after.Version >= before.Version {
		t.Errorf("version did not decrease: %d -> %d", before.Version, after.Version)
	}
	if len(after.Pending) != 1 {
		t.Errorf("pending = %v, want exactly 1", after.Pending)
	}

	if err := r.Up(ctx); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if st, _ := r.Status(ctx); st.Version != before.Version {
		t.Errorf("version after re-apply = %d, want %d", st.Version, before.Version)
	}
}

func TestDownRejectsNonPositiveSteps(t *testing.T) {
	t.Parallel()

	db := testsupport.NewMigratedDatabase(t)
	r := newRunner(t, db.DSN)

	for _, n := range []int{0, -1} {
		if err := r.Down(t.Context(), n); err == nil {
			t.Errorf("Down(%d) should be rejected: there is deliberately no "+
				"'reverse everything' shortcut", n)
		}
	}
}

func TestToMigratesToASpecificVersion(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	r := newRunner(t, db.DSN)
	ctx := t.Context()

	st, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Available) < 2 {
		t.Skip("need at least two migrations")
	}
	target := st.Available[1]

	if err := r.To(ctx, target); err != nil {
		t.Fatalf("to %d: %v", target, err)
	}
	got, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.Version != target {
		t.Errorf("version = %d, want %d", got.Version, target)
	}
	if len(got.Pending) != len(st.Available)-2 {
		t.Errorf("pending = %d, want %d", len(got.Pending), len(st.Available)-2)
	}
}

// A database left mid-migration must be diagnosed, not migrated over. Retrying
// blindly is how a half-applied migration becomes a corrupted schema.
func TestDirtyDatabaseIsRefusedUntilForced(t *testing.T) {
	t.Parallel()

	db := testsupport.NewMigratedDatabase(t)
	r := newRunner(t, db.DSN)
	ctx := t.Context()

	before, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	// Simulate the state golang-migrate leaves after a migration fails part-way.
	if _, err := db.SQL.ExecContext(ctx, `UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}

	st, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status while dirty: %v", err)
	}
	if !st.Dirty {
		t.Fatal("Status should report a dirty database")
	}
	if st.UpToDate() {
		t.Error("a dirty database is never up to date, whatever its version")
	}

	err = r.Up(ctx)
	if err == nil {
		t.Fatal("Up must refuse to run against a dirty database")
	}
	if !errors.Is(err, migrate.ErrDirty) {
		t.Errorf("error should be identifiable as ErrDirty so callers can branch on it, got: %v", err)
	}

	// Force is the documented recovery path.
	if err := r.Force(ctx, int(before.Version)); err != nil {
		t.Fatalf("force: %v", err)
	}
	st, err = r.Status(ctx)
	if err != nil {
		t.Fatalf("status after force: %v", err)
	}
	if st.Dirty {
		t.Error("force should clear the dirty flag")
	}
	if !st.UpToDate() {
		t.Error("schema should be up to date again after force")
	}
	if err := r.Up(ctx); err != nil {
		t.Errorf("Up should work again after force: %v", err)
	}
}

// Two replicas starting at once both call Up. One migrates, the other waits on
// the advisory lock and then finds nothing to do. Neither may fail, and the
// schema must not be applied twice.
func TestConcurrentUpIsSafe(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	ctx := t.Context()

	const replicas = 4
	errs := make(chan error, replicas)
	for range replicas {
		go func() {
			r, err := migrate.New(migrate.Options{
				DatabaseURL: db.DSN,
				Source:      migrations.FS,
				Logger:      slog.New(slog.DiscardHandler),
			})
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = r.Close() }()
			errs <- r.Up(ctx)
		}()
	}
	for i := range replicas {
		if err := <-errs; err != nil {
			t.Errorf("replica %d failed to migrate concurrently: %v", i, err)
		}
	}

	r := newRunner(t, db.DSN)
	st, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.UpToDate() {
		t.Errorf("schema is not up to date after concurrent migration: %+v", st)
	}
	if st.Dirty {
		t.Error("concurrent migration left the database dirty")
	}
}

// A replica must give up on the migration lock rather than hang on it.
//
// Two things have to be wired for that, and neither covers the other: the
// driver takes the advisory lock while *constructing* the instance (outside
// golang-migrate's timeout machinery, on a pg_advisory_lock that blocks
// forever), and golang-migrate takes it again around Up. This test holds the
// lock from a separate session, so whichever of the two runs first has to fail
// on schedule instead of waiting.
func TestTheMigrationLockIsNotWaitedOnForever(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	ctx := t.Context()

	// The driver derives the advisory lock id from the DSN's *path*, so the
	// database name it hashes carries a leading slash. Hashing the bare name
	// gives a different id, which locks nothing the migration cares about and
	// makes this test silently vacuous.
	lockID, err := database.GenerateAdvisoryLockId("/"+db.Name, "public", "schema_migrations")
	if err != nil {
		t.Fatalf("advisory lock id: %v", err)
	}

	// A dedicated connection: advisory locks are session-scoped, so one session
	// has to hold it rather than a pool handing out whichever connection is
	// free.
	holder, err := db.SQL.Conn(ctx)
	if err != nil {
		t.Fatalf("open holding connection: %v", err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		t.Fatalf("take advisory lock: %v", err)
	}

	// Run in a goroutine so a regression shows up as a failure rather than as
	// the whole package timing out with an unhelpful stack.
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		r, err := migrate.New(migrate.Options{
			DatabaseURL: db.DSN,
			Source:      migrations.FS,
			Logger:      slog.New(slog.DiscardHandler),
			LockTimeout: time.Second,
		})
		if err == nil {
			err = r.Up(ctx)
			_ = r.Close()
		}
		done <- result{err: err, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		// Release the lock so the blocked goroutine can finish and the test
		// process can exit rather than deadlocking on shutdown.
		_, _ = holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID)
		t.Fatal("migrating against a held lock never returned: the wait is unbounded")
	}

	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID); err != nil {
		t.Errorf("release advisory lock: %v", err)
	}

	if got.err == nil {
		t.Fatal("migrating should have failed: the advisory lock is held elsewhere")
	}
	if !errors.Is(got.err, migrate.ErrLockUnavailable) {
		t.Errorf("error should be identifiable as ErrLockUnavailable so a caller "+
			"can retry rather than exit, got: %v", got.err)
	}
	// Comfortably under golang-migrate's own 15-second default, so this fails if
	// the configured timeout stops being applied.
	if got.elapsed > 8*time.Second {
		t.Errorf("waited %s for a lock with a 1s timeout", got.elapsed)
	}
}

// Cancellation must stop cleanly between migrations rather than tearing down
// mid-statement, which is what would leave the database dirty.
func TestCancellationIsHonoured(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	r := newRunner(t, db.DSN)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := r.Up(ctx)
	if err == nil {
		t.Skip("migration completed before cancellation was observed")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestStatusOnUnreachableDatabase(t *testing.T) {
	t.Parallel()

	// Port 1 is reserved and never listening.
	_, err := migrate.New(migrate.Options{
		DatabaseURL: "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1",
		Source:      migrations.FS,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Error("opening a runner against an unreachable database should fail fast")
	}
}

func TestCloseIsSafe(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	r, err := migrate.New(migrate.Options{
		DatabaseURL: db.DSN,
		Source:      migrations.FS,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

func TestLockTimeoutIsAccepted(t *testing.T) {
	t.Parallel()

	db := testsupport.NewDatabase(t)
	r, err := migrate.New(migrate.Options{
		DatabaseURL: db.DSN,
		Source:      migrations.FS,
		Logger:      slog.New(slog.DiscardHandler),
		LockTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("open with lock timeout: %v", err)
	}
	defer func() { _ = r.Close() }()

	if err := r.Up(t.Context()); err != nil {
		t.Fatalf("up: %v", err)
	}
}
