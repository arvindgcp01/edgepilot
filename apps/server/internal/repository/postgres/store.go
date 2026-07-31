// Package postgres implements the domain repository interfaces over
// sqlc-generated query code.
//
// Two things in here carry most of the weight:
//
//   - Every query runs inside a transaction that has issued
//     `SET LOCAL edgepilot.org_id`. Row-level security keys off that setting,
//     so a query that runs outside one sees nothing. See scoped().
//
//   - Driver errors are translated into the domain taxonomy at this boundary.
//     Nothing above this package should ever see a pgconn.PgError, and nothing
//     should have to know that 23505 means "duplicate".
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

// Store is the Postgres implementation of domain.Store.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// tx is set on the Store returned inside InTx. When present, every
	// repository uses it instead of opening its own transaction, which is what
	// makes a multi-repository operation atomic.
	tx pgx.Tx
}

// Options configures a Store.
type Options struct {
	DatabaseURL     string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// StatementTimeout bounds any single query, so one pathological plan cannot
	// hold a connection until the pool is exhausted.
	StatementTimeout time.Duration
	Logger           *slog.Logger
}

// New opens a connection pool and verifies it works.
//
// Verifying at construction matters: a pool is lazy, so without a ping the
// first sign of a bad DSN or an unreachable database would be a failed request
// rather than a failed startup.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.DatabaseURL == "" {
		return nil, errors.New("postgres: DatabaseURL is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	cfg, err := pgxpool.ParseConfig(opts.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	if opts.StatementTimeout > 0 {
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", opts.StatementTimeout.Milliseconds())
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	if err := verifyRoleMembership(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return &Store{pool: pool, log: log}, nil
}

// Close releases the pool. Safe on a transaction-scoped Store, where it is a
// no-op: the transaction's lifetime belongs to InTx.
func (s *Store) Close() error {
	if s.tx != nil {
		return nil
	}
	s.pool.Close()
	return nil
}

// Pool exposes the underlying pool for health checks and for the metric ingest
// path, which uses COPY directly rather than going through a repository.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping reports whether the database is reachable.
//
// pgxpool.Ping acquires a connection and issues an empty query, so it exercises
// the path a real request takes — including the pool being exhausted, which is a
// perfectly good reason to take a replica out of the load balancer.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return domain.Internal(fmt.Errorf("ping the database: %w", err))
	}
	return nil
}

// Repository accessors.
func (s *Store) Organizations() domain.OrganizationRepository           { return &organizationRepo{s} }
func (s *Store) Users() domain.UserRepository                           { return &userRepo{s} }
func (s *Store) Roles() domain.RoleRepository                           { return &roleRepo{s} }
func (s *Store) Sessions() domain.SessionRepository                     { return &sessionRepo{s} }
func (s *Store) Devices() domain.DeviceRepository                       { return &deviceRepo{s} }
func (s *Store) DeviceCertificates() domain.DeviceCertificateRepository { return &certRepo{s} }
func (s *Store) EnrollmentTokens() domain.EnrollmentTokenRepository     { return &enrollmentTokenRepo{s} }

// InTx runs fn against a Store whose repositories share one transaction.
//
// Nesting reuses the outer transaction rather than opening a second one:
// pgx would happily give you an independent transaction on another connection,
// which would deadlock against the outer one's locks.
func (s *Store) InTx(ctx context.Context, fn func(domain.Store) error) error {
	if s.tx != nil {
		return fn(s)
	}

	orgID, scoped := domain.OrgFromContext(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Internal(fmt.Errorf("begin transaction: %w", err))
	}
	// Rollback after a successful commit is a no-op that returns
	// pgx.ErrTxClosed, so the deferred call needs no success flag.
	defer func() { _ = tx.Rollback(ctx) }()

	if scoped {
		if err := setOrgScope(ctx, tx, orgID); err != nil {
			return err
		}
	} else if err := setAdminRole(ctx, tx); err != nil {
		return err
	}

	if err := fn(&Store{pool: s.pool, log: s.log, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Internal(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// --- tenant scoping --------------------------------------------------------

// scoped runs fn inside a transaction scoped to the organization in ctx.
//
// This is the single chokepoint every tenant-scoped query passes through. The
// transaction is not optional: `SET LOCAL` only lives for the duration of one,
// so a query issued outside a transaction would run with no scope, and RLS
// would correctly return nothing — a silent empty result rather than an error.
func (s *Store) scoped(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		// A programming error, not a client error: some call site forgot to
		// scope the context. Failing loudly names the bug; letting RLS return
		// an empty set would hide it behind a plausible-looking "no results".
		return domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before using a scoped repository"))
	}
	return s.withTx(ctx, orgID, fn)
}

// unscoped runs fn without a tenant scope, for the few operations that
// legitimately cross organizations.
//
// There are exactly three: redeeming an enrollment token (the agent has no
// organization yet — the token is what establishes one), reading the
// certificate revocation set (TLS verification happens before any application
// identity exists), and the background workers. Everything else uses scoped().
func (s *Store) unscoped(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	return s.withTx(ctx, uuid.Nil, fn)
}

func (s *Store) withTx(ctx context.Context, orgID uuid.UUID, fn func(*sqlcgen.Queries) error) error {
	if s.tx != nil {
		// Already inside InTx; the scope was set when the transaction opened.
		return fn(sqlcgen.New(s.tx))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Internal(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if orgID != uuid.Nil {
		if err := setOrgScope(ctx, tx, orgID); err != nil {
			return err
		}
	} else if err := setAdminRole(ctx, tx); err != nil {
		return err
	}

	if err := fn(sqlcgen.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Internal(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

// Database roles the transaction assumes. See setOrgScope.
const (
	roleApp   = "edgepilot_app"
	roleAdmin = "edgepilot_admin"
)

// setOrgScope makes the transaction subject to row-level security and tells the
// policies which organization it is scoped to.
//
// The `SET LOCAL ROLE` is not decoration. PostgreSQL exempts superusers and
// roles holding BYPASSRLS from every policy, so a pool that happens to connect
// as a privileged role — which is the default in most development setups, and
// one misconfigured DSN away in production — would silently see every tenant's
// data with all the policies still in place and apparently working.
//
// Assuming the restricted role inside the transaction makes isolation a
// property of this code rather than of the deployment's connection string.
// Verified by TestDeviceIsInvisibleToAnotherTenant, which caught exactly this:
// the policies were correct and doing nothing.
//
// set_config with is_local=true is used for the org id rather than `SET LOCAL`
// because the value is a parameter, and `SET LOCAL` takes no placeholders —
// building that statement by concatenation would put a value into SQL text.
func setOrgScope(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+roleApp); err != nil {
		return domain.Internal(fmt.Errorf("assume %s role: %w", roleApp, err))
	}
	if _, err := tx.Exec(ctx,
		`SELECT set_config('edgepilot.org_id', $1, true)`, orgID.String()); err != nil {
		return domain.Internal(fmt.Errorf("set tenant scope: %w", err))
	}
	return nil
}

// setAdminRole assumes the cross-organization role for the few operations that
// legitimately span tenants.
//
// Stating it explicitly, rather than simply not scoping, means "this query
// crosses tenants" is visible in the database session as well as in the Go
// code — and that anything which forgets to declare either intent fails rather
// than defaulting to whatever the pool happened to connect as.
func setAdminRole(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+roleAdmin); err != nil {
		return domain.Internal(fmt.Errorf("assume %s role: %w", roleAdmin, err))
	}
	return nil
}

// verifyRoleMembership checks at startup that the connecting role can assume
// both application roles.
//
// Failing here is the point: without membership every query would fail at
// runtime with "permission denied to set role", and the alternative — skipping
// the role change — would mean running with no tenant isolation at all. A
// refused startup is the only safe outcome.
func verifyRoleMembership(ctx context.Context, pool *pgxpool.Pool) error {
	for _, role := range []string{roleApp, roleAdmin} {
		var member bool
		err := pool.QueryRow(ctx,
			`SELECT pg_has_role(current_user, $1, 'member')`, role).Scan(&member)
		if err != nil {
			return fmt.Errorf("postgres: check %s membership: %w", role, err)
		}
		if !member {
			return fmt.Errorf(
				"postgres: the connecting role is not a member of %s. "+
					"Tenant isolation depends on assuming it, so the server will not start. "+
					"Run: GRANT %s TO <login role>", role, role)
		}
	}
	return nil
}

// compile-time assertion that the Postgres implementation satisfies the domain
// contract. Without it, a signature drift would only surface where a Store is
// first assigned to the interface.
var _ domain.Store = (*Store)(nil)
