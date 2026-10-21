package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// RefreshReported records the facts an agent sends in Hello.
//
// Hand-written rather than generated for the same reason as the SSH
// repositories: it arrived after the sqlc surface was settled, and regenerating
// everything for one statement would touch far more than it needs to. The
// transaction and role discipline is identical.
//
// # NULLIF, and why empty is not a value
//
// Every column is `COALESCE(NULLIF($n, ”), column)`. An agent that cannot
// determine its OS build sends an empty string, and writing that would erase a
// value the fleet already knew — making the listing worse on every reconnect
// rather than better. Empty means "no opinion", not "unset it".
//
// Callers are expected to have checked ReportedFacts.DiffersFrom first. This is
// still safe to call unconditionally; it would simply be a write that changes
// nothing, on every connect, for every device.
func (r *deviceRepo) RefreshReported(ctx context.Context, id uuid.UUID, in domain.ReportedFacts) error {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before refreshing device facts"))
	}

	return r.s.rawScoped(ctx, orgID, func(tx pgx.Tx) error {
		// local_ip is inet, so it is cast separately — an empty string is not a
		// valid inet and would fail the cast rather than coalesce away.
		_, err := tx.Exec(ctx, `
			UPDATE devices
			   SET hostname      = COALESCE(NULLIF($2, ''), hostname),
			       os_version    = COALESCE(NULLIF($3, ''), os_version),
			       os_build      = COALESCE(NULLIF($4, ''), os_build),
			       agent_version = COALESCE(NULLIF($5, ''), agent_version),
			       local_ip      = COALESCE(NULLIF($6, '')::inet, local_ip)
			 WHERE id = $1 AND deleted_at IS NULL`,
			id, in.Hostname, in.OSVersion, in.OSBuild, in.AgentVersion, in.LocalIP)
		return translate(err, "device")
	})
}
