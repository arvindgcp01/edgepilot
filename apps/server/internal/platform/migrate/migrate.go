// Package migrate applies EdgePilot's database schema.
//
// Migrations are explicit, versioned SQL — never GORM's AutoMigrate. A
// production schema change has to be reviewable in a diff, reversible, and
// byte-identical across development, CI, and production; reflection over Go
// structs gives none of those, and it will happily drop a column because
// somebody deleted a field.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"

	// Registers the "pgx5" URL scheme. pgx is used rather than lib/pq because it
	// is actively maintained and because the rest of the server needs it anyway
	// — one Postgres driver in the binary, not two.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
)

// ErrDirty reports a database left mid-migration by an earlier failure.
//
// It is deliberately its own error: recovering needs a human to look at what
// actually landed, and retrying blindly is how a half-applied migration becomes
// a corrupted schema.
var ErrDirty = errors.New("database is in a dirty migration state")

// ErrLockUnavailable reports that another process holds the migration lock for
// longer than this one was willing to wait.
//
// Distinct from a failed migration: nothing was applied, the schema is intact,
// and retrying is the right response. A replica starting during a long
// migration should back off and try again, not exit non-zero and be replaced by
// an identical replica that does the same thing.
var ErrLockUnavailable = errors.New("the migration lock is held by another process")

// lockNotAvailable is SQLSTATE 55P03, which PostgreSQL raises when lock_timeout
// expires while waiting for a lock — including an advisory one.
const lockNotAvailable = "55P03"

// Runner applies and reports on migrations.
type Runner struct {
	m      *migrate.Migrate
	source fs.FS
	log    *slog.Logger
}

// Options configures a Runner.
type Options struct {
	// DatabaseURL is a postgres:// DSN.
	DatabaseURL string
	// Source holds the migration files, normally migrations.FS.
	Source fs.FS
	// Logger receives progress. Defaults to slog.Default().
	Logger *slog.Logger
	// LockTimeout bounds the wait for the advisory lock that serialises
	// migrations across replicas. Zero means DefaultLockTimeout.
	LockTimeout time.Duration
}

// DefaultLockTimeout is how long a replica waits for another replica's
// migration to finish before giving up.
//
// golang-migrate's own default is 15 seconds, which is wrong for the way this
// server starts: every replica calls Up() at boot, one wins the advisory lock,
// and the rest wait. A migration that adds an index to a large table takes
// minutes, so a 15-second ceiling means every replica but one fails to start
// and crash-loops through the deploy that needed the migration most.
//
// Five minutes is long enough for a real migration and short enough that a
// genuinely stuck lock still surfaces as a failed deploy rather than a hang.
// Anything longer belongs in an explicit LockTimeout.
const DefaultLockTimeout = 5 * time.Minute

// New opens a Runner. Close it when finished.
func New(opts Options) (*Runner, error) {
	if opts.DatabaseURL == "" {
		return nil, errors.New("migrate: DatabaseURL is required")
	}
	if opts.Source == nil {
		return nil, errors.New("migrate: Source is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	src, err := iofs.New(opts.Source, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migration source: %w", err)
	}

	lockTimeout := opts.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}

	// golang-migrate's pgx driver takes a session-level advisory lock for the
	// duration of a run. That is what makes it safe for every replica to call
	// Up() at startup: one migrates, the rest wait and then find nothing to do.
	//
	// Bounding that wait takes two settings, because there are two places the
	// lock is taken and neither covers the other:
	//
	//   - PostgreSQL's own lock_timeout, set on the connection. The driver takes
	//     the advisory lock while *constructing* the instance, to create the
	//     version table, and that acquisition runs outside golang-migrate's
	//     timeout machinery entirely — pg_advisory_lock blocks forever, so
	//     without this a replica starting during another's migration hangs at
	//     startup with nothing in its logs to say why.
	//
	//   - m.LockTimeout, which bounds the acquisition around Up/Down. That one
	//     is a field, not a DSN parameter: no driver reads x-lock-timeout, so
	//     passing it looks configured while leaving the 15-second default.
	//
	// lock_timeout also applies to ordinary locks taken by the migration
	// statements themselves. That is intentional: a migration blocked for this
	// long behind another transaction is stalling the deploy and queuing every
	// request behind it, and failing loudly beats hanging silently.
	dsn := withQueryParam(ensurePgxScheme(opts.DatabaseURL), "lock_timeout",
		strconv.FormatInt(lockTimeout.Milliseconds(), 10))

	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		if isLockTimeout(err) {
			return nil, fmt.Errorf("%w after %s: another replica is probably "+
				"still migrating", ErrLockUnavailable, lockTimeout)
		}
		return nil, fmt.Errorf("migrate: open: %w", err)
	}
	m.Log = migrateLogger{log: log}
	m.LockTimeout = lockTimeout

	return &Runner{m: m, source: opts.Source, log: log}, nil
}

// Up applies every pending migration.
func (r *Runner) Up(ctx context.Context) error {
	before, _, _ := r.m.Version()

	if err := r.run(ctx, r.m.Up); err != nil {
		return err
	}

	after, _, err := r.m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("migrate: read version: %w", err)
	}
	if before == after {
		r.log.InfoContext(ctx, "schema is up to date", slog.Uint64("version", uint64(after)))
	} else {
		r.log.InfoContext(ctx, "schema migrated",
			slog.Uint64("from", uint64(before)), slog.Uint64("to", uint64(after)))
	}
	return nil
}

// Down reverses the most recent n migrations. n must be positive; there is no
// "reverse everything" shortcut, because that is not something anyone should be
// one typo away from.
func (r *Runner) Down(ctx context.Context, n int) error {
	if n <= 0 {
		return fmt.Errorf("migrate: steps must be positive, got %d", n)
	}
	return r.run(ctx, func() error { return r.m.Steps(-n) })
}

// To migrates up or down to a specific version.
func (r *Runner) To(ctx context.Context, version uint) error {
	return r.run(ctx, func() error { return r.m.Migrate(version) })
}

// Force sets the recorded version without running anything, clearing the dirty
// flag.
//
// This is a recovery tool, and it is dangerous: it tells the database "trust me,
// the schema is at version N". Use it only after inspecting what the failed
// migration actually applied.
func (r *Runner) Force(ctx context.Context, version int) error {
	r.log.WarnContext(ctx, "forcing migration version without applying any SQL",
		slog.Int("version", version))
	if err := r.m.Force(version); err != nil {
		return fmt.Errorf("migrate: force to %d: %w", version, err)
	}
	return nil
}

// Status describes the current schema state.
type Status struct {
	// Version is the highest applied migration, or 0 when none have been.
	Version uint
	// Dirty reports a database left mid-migration by an earlier failure.
	Dirty bool
	// Available is every migration version found in the source, ascending.
	Available []uint
	// Pending is the subset of Available not yet applied.
	Pending []uint
}

// UpToDate reports whether the schema is current and clean.
func (s Status) UpToDate() bool { return !s.Dirty && len(s.Pending) == 0 }

// Status reads the applied version and compares it against the embedded source.
func (r *Runner) Status(ctx context.Context) (Status, error) {
	var st Status

	version, dirty, err := r.m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		// A database with no migrations applied. Not an error: it is the state
		// of every new deployment.
		st.Version = 0
	case err != nil:
		return st, fmt.Errorf("migrate: read version: %w", err)
	default:
		st.Version = version
	}
	st.Dirty = dirty

	st.Available, err = availableVersions(r.source)
	if err != nil {
		return st, err
	}
	for _, v := range st.Available {
		if v > st.Version {
			st.Pending = append(st.Pending, v)
		}
	}

	_ = ctx // reserved: golang-migrate's Version() is not context-aware
	return st, nil
}

// Close releases the database handles.
func (r *Runner) Close() error {
	sourceErr, dbErr := r.m.Close()
	return errors.Join(sourceErr, dbErr)
}

// run wraps a migration operation with dirty-state detection and no-change
// handling.
func (r *Runner) run(ctx context.Context, op func() error) error {
	done := make(chan error, 1)
	go func() { done <- op() }()

	select {
	case err := <-done:
		// Dirty state is detected by golang-migrate itself, which checks it
		// while holding the advisory lock. Checking here beforehand would race:
		// a replica that reads the flag while a peer is legitimately mid-
		// migration would report corruption where there is none.
		var dirty migrate.ErrDirty
		switch {
		case err == nil, errors.Is(err, migrate.ErrNoChange):
			return nil
		case errors.Is(err, migrate.ErrLockTimeout), isLockTimeout(err):
			return fmt.Errorf("%w after %s: nothing was applied, and retrying "+
				"once the other migration finishes is safe", ErrLockUnavailable, r.m.LockTimeout)
		case errors.As(err, &dirty):
			return fmt.Errorf("%w at version %d: inspect what that migration applied, "+
				"then run `epctl migrate force <version>` to record the true state",
				ErrDirty, dirty.Version)
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("migrate: a migration file is missing: %w", err)
		default:
			return fmt.Errorf("migrate: %w", err)
		}
	case <-ctx.Done():
		// GracefulStop asks golang-migrate to stop after the current migration
		// rather than tearing down mid-statement.
		r.m.GracefulStop <- true
		<-done
		return ctx.Err()
	}
}

// availableVersions lists migration versions present in the source.
func availableVersions(source fs.FS) ([]uint, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: list source: %w", err)
	}

	seen := map[uint]struct{}{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		underscore := strings.IndexByte(name, '_')
		if underscore <= 0 {
			continue
		}
		v, err := strconv.ParseUint(name[:underscore], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migrate: %q does not start with a numeric version", name)
		}
		seen[uint(v)] = struct{}{}
	}

	out := make([]uint, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ensurePgxScheme rewrites postgres:// to pgx5:// so golang-migrate selects the
// pgx driver registered by the blank import above.
func ensurePgxScheme(dsn string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(dsn, prefix) {
			return "pgx5://" + strings.TrimPrefix(dsn, prefix)
		}
	}
	return dsn
}

func withQueryParam(dsn, key, value string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + key + "=" + value
}

// isLockTimeout reports whether an error is PostgreSQL giving up on a lock.
//
// golang-migrate's database.Error carries the driver error in a field rather
// than implementing Unwrap, so errors.As cannot reach the pg error through it
// and it has to be unpacked by hand.
func isLockTimeout(err error) bool {
	var dbErr *database.Error
	if errors.As(err, &dbErr) && dbErr.OrigErr != nil {
		err = dbErr.OrigErr
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// migrateLogger adapts slog to golang-migrate's logger interface.
type migrateLogger struct{ log *slog.Logger }

func (l migrateLogger) Printf(format string, v ...any) {
	l.log.Debug(strings.TrimSpace(fmt.Sprintf(format, v...)))
}

// Verbose reports whether golang-migrate should emit progress. Tied to the
// logger's own level so one setting controls both.
func (l migrateLogger) Verbose() bool {
	return l.log.Enabled(context.Background(), slog.LevelDebug)
}

// compile-time assertion that the slog adapter satisfies golang-migrate.
var _ migrate.Logger = migrateLogger{}
