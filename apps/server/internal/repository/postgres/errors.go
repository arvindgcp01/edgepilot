package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// PostgreSQL error codes worth distinguishing.
// https://www.postgresql.org/docs/16/errcodes-appendix.html
const (
	codeUniqueViolation       = "23505"
	codeForeignKeyViolation   = "23503"
	codeCheckViolation        = "23514"
	codeNotNullViolation      = "23502"
	codeExclusionViolation    = "23P01"
	codeInsufficientPrivilege = "42501"
	codeSerializationFailure  = "40001"
	codeDeadlockDetected      = "40P01"
	codeQueryCanceled         = "57014"
	codeNoPartitionFound      = "23514" // surfaces as a check violation
)

// translate converts a driver error into the domain taxonomy.
//
// This is the only place in the server that knows what "23505" means. Above
// this line the code deals in domain.ErrConflict and domain.ErrNotFound, which
// is what lets the HTTP layer map errors without importing a database driver.
//
// resource names what was being operated on, for the not-found message.
func translate(err error, resource string) error {
	if err == nil {
		return nil
	}

	// Already ours — a validation error raised inside a repository callback,
	// for instance. Do not re-wrap.
	if _, ok := domain.AsError(err); ok {
		return err
	}

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// This is also what a row-level-security denial looks like: the policy
		// filters the row out, so the query returns nothing rather than an
		// error. Reporting it as not-found is exactly right — a resource in
		// another organization must be indistinguishable from one that does not
		// exist, or the API becomes an oracle for enumerating another tenant.
		return domain.NotFound(resource, "")

	case errors.Is(err, context.Canceled):
		return domain.Unavailable("request_canceled", "The request was cancelled.").Wrap(err)

	case errors.Is(err, context.DeadlineExceeded):
		return domain.Unavailable("database_timeout", "The database did not respond in time.").Wrap(err)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return domain.Internal(err)
	}

	switch pgErr.Code {
	case codeUniqueViolation:
		return domain.Conflict(conflictCode(pgErr.ConstraintName, resource),
			"%s", uniqueMessage(pgErr.ConstraintName, resource)).Wrap(err)

	case codeForeignKeyViolation:
		// A foreign key failure on write is nearly always a reference to
		// something that does not exist — or that the caller cannot see, which
		// under RLS is the same thing.
		return domain.Conflict("reference_not_found",
			"A referenced record does not exist or is not visible.").Wrap(err)

	case codeCheckViolation:
		// The database rejected a value the service layer should have caught.
		// It is reported as a validation failure rather than a 500 because the
		// caller can act on it, but the wrapped cause is logged: reaching here
		// means a validation rule is missing upstream.
		return domain.Validation(domain.FieldError{
			Pointer: "",
			Message: checkMessage(pgErr.ConstraintName),
		}).Wrap(err)

	case codeNotNullViolation:
		return domain.Invalid("/"+pgErr.ColumnName, "is required").Wrap(err)

	case codeExclusionViolation:
		return domain.Conflict("overlapping_record", "The record conflicts with an existing one.").Wrap(err)

	case codeInsufficientPrivilege:
		// The application role lacks a GRANT. Not a client problem: it means a
		// migration added a table without granting on it.
		return domain.Internal(fmt.Errorf(
			"database privilege missing (a migration likely added a table without a GRANT): %w", err))

	case codeSerializationFailure, codeDeadlockDetected:
		// Retryable. Surfaced as unavailable so the caller can back off rather
		// than treating a transient contention as a permanent failure.
		return domain.Unavailable("database_contention",
			"The request conflicted with another and can be retried.").Wrap(err)

	case codeQueryCanceled:
		return domain.Unavailable("database_timeout",
			"The query exceeded its time limit.").Wrap(err)
	}

	return domain.Internal(err)
}

// conflictCode derives a stable machine-readable code from the constraint that
// failed, so a client can branch on "this serial is already enrolled" without
// parsing prose.
func conflictCode(constraint, resource string) string {
	switch constraint {
	case "devices_org_serial_key":
		return "serial_already_enrolled"
	case "users_org_email_key":
		return "email_already_in_use"
	case "organizations_slug_key":
		return "slug_already_in_use"
	case "software_packages_org_slug_key":
		return "package_slug_already_in_use"
	case "sessions_refresh_token_hash_key":
		return "session_already_exists"
	case "alerts_fingerprint_active_key":
		return "alert_already_firing"
	case "enrollment_tokens_token_hash_key":
		return "token_already_exists"
	case "":
		return resource + "_conflict"
	default:
		return resource + "_conflict"
	}
}

func uniqueMessage(constraint, resource string) string {
	switch constraint {
	case "devices_org_serial_key":
		return "A device with this serial number is already enrolled."
	case "users_org_email_key":
		return "A user with this email address already exists."
	case "organizations_slug_key":
		return "This slug is already in use."
	case "alerts_fingerprint_active_key":
		return "An unresolved alert already exists for this condition."
	default:
		if resource != "" {
			return "This " + resource + " already exists."
		}
		return "The record already exists."
	}
}

// checkMessage turns a constraint name into something an operator can act on.
//
// The constraints named here are the ones a caller can actually trip; anything
// else gets a generic message, because a check the service layer should have
// enforced is a bug to fix rather than a message to write.
func checkMessage(constraint string) string {
	switch {
	case constraint == "deployments_check1":
		return "A rolling deployment requires a batch size."
	case constraint == "deployments_check":
		return "A scheduled deployment requires a scheduled time."
	case constraint == "commands_check":
		return "A custom script command requires a script, and other commands must not carry one."
	case constraint == "files_check":
		return "A file transfer to or from a device requires both a device and a remote path."
	case strings.HasSuffix(constraint, "_check"):
		return "A value in the request is not permitted."
	default:
		return "A value in the request is not permitted."
	}
}

// notFound builds a not-found error naming the identifier the caller supplied,
// for the cases where the repository knows it.
func notFound(resource, id string) error { return domain.NotFound(resource, id) }

// errNoTenantScope names the programming error of using a scoped repository
// without first calling domain.WithOrg.
var errNoTenantScope = errors.New(
	"tenant scope missing from context: call domain.WithOrg before using a scoped repository")
