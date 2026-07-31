package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

type sessionRepo struct{ s *Store }

// Sessions run unscoped. They are read during authentication, before a scope
// exists — a refresh token is presented by a client that has no valid access
// token, so there is no organization to scope by until the session itself
// supplies one.

func (r *sessionRepo) Create(ctx context.Context, in *domain.Session, tokenHash []byte) (*domain.Session, error) {
	if len(tokenHash) != 32 {
		return nil, domain.Internal(errBadTokenHash)
	}
	var out *domain.Session
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.CreateSession(ctx, sqlcgen.CreateSessionParams{
			UserID:           toPgUUID(in.UserID),
			OrgID:            toPgUUID(in.OrgID),
			RefreshTokenHash: tokenHash,
			FamilyID:         toPgUUID(in.FamilyID),
			ParentID:         toPgUUIDPtr(in.ParentID),
			UserAgent:        in.UserAgent,
			Ip:               toNetip(in.IP),
			ExpiresAt:        toPgTime(in.ExpiresAt),
		})
		if err != nil {
			return translate(err, "session")
		}
		out = sessionFromRow(row)
		return nil
	})
	return out, err
}

func (r *sessionRepo) GetByTokenHash(ctx context.Context, tokenHash []byte) (*domain.Session, error) {
	var out *domain.Session
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetSessionByTokenHash(ctx, tokenHash)
		if err != nil {
			return translate(err, "session")
		}
		// sqlc emits a distinct row type per query even when the columns are
		// identical; the conversion is what keeps one mapping function rather
		// than one per query.
		out = sessionFromRow(sqlcgen.CreateSessionRow(row))
		return nil
	})
	return out, err
}

// Rotate revokes the presented session and issues its successor atomically.
//
// Two separate calls would leave a window where a crash between them loses the
// user's session entirely: the old token revoked, the new one never created,
// and no way to recover but signing in again.
func (r *sessionRepo) Rotate(ctx context.Context, oldID uuid.UUID, next *domain.Session, nextHash []byte) (*domain.Session, error) {
	if len(nextHash) != 32 {
		return nil, domain.Internal(errBadTokenHash)
	}
	var out *domain.Session
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.RevokeSession(ctx, sqlcgen.RevokeSessionParams{
			ID:     toPgUUID(oldID),
			Reason: ptrString("rotated"),
		})
		if err != nil {
			return translate(err, "session")
		}
		if n == 0 {
			// Already revoked. The caller reached here holding a token that was
			// rotated before, which is the reuse signal — it must not be allowed
			// to mint a fresh session.
			return domain.Conflict("session_already_rotated",
				"This refresh token has already been used.")
		}

		row, err := q.CreateSession(ctx, sqlcgen.CreateSessionParams{
			UserID:           toPgUUID(next.UserID),
			OrgID:            toPgUUID(next.OrgID),
			RefreshTokenHash: nextHash,
			FamilyID:         toPgUUID(next.FamilyID),
			ParentID:         toPgUUIDPtr(&oldID),
			UserAgent:        next.UserAgent,
			Ip:               toNetip(next.IP),
			ExpiresAt:        toPgTime(next.ExpiresAt),
		})
		if err != nil {
			return translate(err, "session")
		}
		out = sessionFromRow(row)
		return nil
	})
	return out, err
}

func (r *sessionRepo) Revoke(ctx context.Context, id uuid.UUID, reason string) error {
	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		// Zero rows means it was already revoked, which is the desired end
		// state — revocation is idempotent by design, so a double logout is not
		// an error.
		if _, err := q.RevokeSession(ctx, sqlcgen.RevokeSessionParams{
			ID:     toPgUUID(id),
			Reason: ptrString(reason),
		}); err != nil {
			return translate(err, "session")
		}
		return nil
	})
}

func (r *sessionRepo) RevokeFamily(ctx context.Context, familyID uuid.UUID, reason string) (int, error) {
	var n int64
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.RevokeSessionFamily(ctx, sqlcgen.RevokeSessionFamilyParams{
			FamilyID: toPgUUID(familyID),
			Reason:   ptrString(reason),
		})
		if err != nil {
			return translate(err, "session")
		}
		return nil
	})
	return int(n), err
}

func (r *sessionRepo) RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string) (int, error) {
	var n int64
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.RevokeUserSessions(ctx, sqlcgen.RevokeUserSessionsParams{
			UserID: toPgUUID(userID),
			Reason: ptrString(reason),
		})
		if err != nil {
			return translate(err, "session")
		}
		return nil
	})
	return int(n), err
}

func (r *sessionRepo) DeleteExpired(ctx context.Context, before time.Time) (int, error) {
	var n int64
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.DeleteExpiredSessions(ctx, toPgTime(before))
		if err != nil {
			return translate(err, "session")
		}
		return nil
	})
	return int(n), err
}

func sessionFromRow(row sqlcgen.CreateSessionRow) *domain.Session {
	return &domain.Session{
		ID:            fromPgUUID(row.ID),
		UserID:        fromPgUUID(row.UserID),
		OrgID:         fromPgUUID(row.OrgID),
		FamilyID:      fromPgUUID(row.FamilyID),
		ParentID:      fromPgUUIDPtr(row.ParentID),
		UserAgent:     row.UserAgent,
		IP:            fromNetip(row.Ip),
		ExpiresAt:     fromPgTime(row.ExpiresAt),
		LastUsedAt:    fromPgTimePtr(row.LastUsedAt),
		RevokedAt:     fromPgTimePtr(row.RevokedAt),
		RevokedReason: derefString(row.RevokedReason),
		CreatedAt:     fromPgTime(row.CreatedAt),
	}
}
