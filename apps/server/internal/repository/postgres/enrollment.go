package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

var errBadTokenHash = errors.New("token hash must be exactly 32 bytes (SHA-256)")

// --- enrollment tokens -----------------------------------------------------

type enrollmentTokenRepo struct{ s *Store }

func (r *enrollmentTokenRepo) Create(ctx context.Context, in domain.CreateEnrollmentTokenInput, tokenHash []byte, prefix string) (*domain.EnrollmentToken, error) {
	if err := in.Validate(time.Now()); err != nil {
		return nil, err
	}
	if len(tokenHash) != 32 {
		return nil, domain.Internal(errBadTokenHash)
	}
	labels, err := labelsToJSON(in.Labels)
	if err != nil {
		return nil, domain.Internal(err)
	}

	var maxUses *int32
	if in.MaxUses != nil {
		n := int32(*in.MaxUses)
		maxUses = &n
	}

	var out *domain.EnrollmentToken
	err = r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.CreateEnrollmentToken(ctx, sqlcgen.CreateEnrollmentTokenParams{
			OrgID:       toPgUUID(in.OrgID),
			TokenHash:   tokenHash,
			TokenPrefix: prefix,
			Description: in.Description,
			MaxUses:     maxUses,
			AutoApprove: in.AutoApprove,
			Labels:      labels,
			ExpiresAt:   toPgTime(in.ExpiresAt),
			CreatedBy:   toPgUUIDPtr(in.CreatedBy),
		})
		if err != nil {
			return translate(err, "enrollment token")
		}
		out = tokenFromRow(row)
		return nil
	})
	return out, err
}

func (r *enrollmentTokenRepo) List(ctx context.Context, page domain.PageRequest) (domain.Page[*domain.EnrollmentToken], error) {
	page = page.Normalize()
	var result domain.Page[*domain.EnrollmentToken]

	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return result, domain.Internal(errNoTenantScope)
	}

	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListEnrollmentTokens(ctx, sqlcgen.ListEnrollmentTokensParams{
			OrgID:      toPgUUID(orgID),
			CursorTime: toPgTime(page.Cursor.Time),
			CursorID:   toPgUUID(page.Cursor.ID),
			PageSize:   int32(page.Limit + 1),
		})
		if err != nil {
			return translate(err, "enrollment token")
		}
		tokens := make([]*domain.EnrollmentToken, 0, len(rows))
		for _, row := range rows {
			tokens = append(tokens, tokenFromRow(sqlcgen.CreateEnrollmentTokenRow(row)))
		}
		result = domain.NewPage(tokens, page.Limit, func(t *domain.EnrollmentToken) domain.Cursor {
			return domain.Cursor{Time: t.CreatedAt, ID: t.ID}
		})
		return nil
	})
	return result, err
}

func (r *enrollmentTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.RevokeEnrollmentToken(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "enrollment token")
		}
		if n == 0 {
			return notFound("enrollment token", id.String())
		}
		return nil
	})
}

// Redeem validates and consumes a token in one atomic statement.
//
// Unscoped by necessity: the agent presenting the token has no organization
// yet. The token is what establishes which one it belongs to, so scoping the
// query by an organization we do not know would be circular.
func (r *enrollmentTokenRepo) Redeem(ctx context.Context, tokenHash []byte, now time.Time) (*domain.EnrollmentToken, error) {
	if len(tokenHash) != 32 {
		return nil, domain.Internal(errBadTokenHash)
	}
	var out *domain.EnrollmentToken
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.RedeemEnrollmentToken(ctx, sqlcgen.RedeemEnrollmentTokenParams{
			TokenHash: tokenHash,
			Now:       toPgTime(now),
		})
		if err != nil {
			// Every failure mode — unknown, revoked, expired, exhausted —
			// returns no row and therefore the same error. That uniformity is
			// deliberate: an agent holding a bad token learns only that it is
			// bad, not which kind of bad, so a probe cannot enumerate valid
			// token prefixes.
			return translate(err, "enrollment token")
		}
		out = tokenFromRow(sqlcgen.CreateEnrollmentTokenRow(row))
		return nil
	})
	return out, err
}

func tokenFromRow(row sqlcgen.CreateEnrollmentTokenRow) *domain.EnrollmentToken {
	var maxUses *int
	if row.MaxUses != nil {
		n := int(*row.MaxUses)
		maxUses = &n
	}
	return &domain.EnrollmentToken{
		ID:          fromPgUUID(row.ID),
		OrgID:       fromPgUUID(row.OrgID),
		TokenPrefix: row.TokenPrefix,
		Description: row.Description,
		MaxUses:     maxUses,
		UseCount:    int(row.UseCount),
		AutoApprove: row.AutoApprove,
		Labels:      labelsFromJSON(row.Labels),
		ExpiresAt:   fromPgTime(row.ExpiresAt),
		RevokedAt:   fromPgTimePtr(row.RevokedAt),
		CreatedBy:   fromPgUUIDPtr(row.CreatedBy),
		CreatedAt:   fromPgTime(row.CreatedAt),
	}
}

// --- device certificates ---------------------------------------------------

type certRepo struct{ s *Store }

func (r *certRepo) Issue(ctx context.Context, cert *domain.DeviceCertificate) (*domain.DeviceCertificate, error) {
	serial, err := numericFromDecimalString(cert.Serial)
	if err != nil {
		return nil, domain.Internal(err)
	}
	var out *domain.DeviceCertificate
	err = r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.IssueDeviceCertificate(ctx, sqlcgen.IssueDeviceCertificateParams{
			DeviceID:          toPgUUID(cert.DeviceID),
			Serial:            serial,
			FingerprintSha256: cert.FingerprintSHA256,
			PublicKeyDer:      cert.PublicKeyDER,
			CertificatePem:    cert.CertificatePEM,
			NotBefore:         toPgTime(cert.NotBefore),
			NotAfter:          toPgTime(cert.NotAfter),
			IssuedVia:         cert.IssuedVia,
		})
		if err != nil {
			return translate(err, "certificate")
		}
		out = certFromRow(row)
		return nil
	})
	return out, err
}

// GetByFingerprint runs unscoped: it is called from the mTLS verification
// callback, which happens before any application-level identity exists.
func (r *certRepo) GetByFingerprint(ctx context.Context, fingerprint []byte) (*domain.DeviceCertificate, error) {
	var out *domain.DeviceCertificate
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetCertificateByFingerprint(ctx, fingerprint)
		if err != nil {
			return translate(err, "certificate")
		}
		out = certFromRow(row)
		return nil
	})
	return out, err
}

func (r *certRepo) ListForDevice(ctx context.Context, deviceID uuid.UUID) ([]*domain.DeviceCertificate, error) {
	var out []*domain.DeviceCertificate
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListCertificatesForDevice(ctx, toPgUUID(deviceID))
		if err != nil {
			return translate(err, "certificate")
		}
		out = make([]*domain.DeviceCertificate, 0, len(rows))
		for _, row := range rows {
			out = append(out, certFromRow(row))
		}
		return nil
	})
	return out, err
}

func (r *certRepo) Current(ctx context.Context, deviceID uuid.UUID) (*domain.DeviceCertificate, error) {
	var out *domain.DeviceCertificate
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetCurrentCertificate(ctx, sqlcgen.GetCurrentCertificateParams{
			DeviceID: toPgUUID(deviceID),
			Now:      toPgTime(time.Now()),
		})
		if err != nil {
			return translate(err, "certificate")
		}
		out = certFromRow(row)
		return nil
	})
	return out, err
}

func (r *certRepo) Revoke(ctx context.Context, id uuid.UUID, reason string, by *uuid.UUID) error {
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.RevokeCertificate(ctx, sqlcgen.RevokeCertificateParams{
			ID:        toPgUUID(id),
			Reason:    ptrString(reason),
			RevokedBy: toPgUUIDPtr(by),
		})
		if err != nil {
			return translate(err, "certificate")
		}
		if n == 0 {
			return notFound("certificate", id.String())
		}
		return nil
	})
}

func (r *certRepo) RevokeAllForDevice(ctx context.Context, deviceID uuid.UUID, reason string) (int, error) {
	var n int64
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.RevokeDeviceCertificates(ctx, sqlcgen.RevokeDeviceCertificatesParams{
			DeviceID: toPgUUID(deviceID),
			Reason:   ptrString(reason),
		})
		if err != nil {
			return translate(err, "certificate")
		}
		return nil
	})
	return int(n), err
}

func (r *certRepo) ListRevokedFingerprints(ctx context.Context) ([][]byte, error) {
	var out [][]byte
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListRevokedFingerprints(ctx, toPgTime(time.Now()))
		if err != nil {
			return translate(err, "certificate")
		}
		out = rows
		return nil
	})
	return out, err
}

func certFromRow(row sqlcgen.DeviceCertificate) *domain.DeviceCertificate {
	return &domain.DeviceCertificate{
		ID:                fromPgUUID(row.ID),
		DeviceID:          fromPgUUID(row.DeviceID),
		Serial:            numericToDecimalString(row.Serial),
		FingerprintSHA256: row.FingerprintSha256,
		PublicKeyDER:      row.PublicKeyDer,
		CertificatePEM:    row.CertificatePem,
		NotBefore:         fromPgTime(row.NotBefore),
		NotAfter:          fromPgTime(row.NotAfter),
		IssuedVia:         row.IssuedVia,
		RevokedAt:         fromPgTimePtr(row.RevokedAt),
		RevokedReason:     derefString(row.RevokedReason),
		RevokedBy:         fromPgUUIDPtr(row.RevokedBy),
		CreatedAt:         fromPgTime(row.CreatedAt),
	}
}
