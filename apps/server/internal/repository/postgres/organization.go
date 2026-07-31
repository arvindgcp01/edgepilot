package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

type organizationRepo struct{ s *Store }

// Organizations are not tenant-scoped: a caller must be able to read its own
// organization before a scope exists to read it under. The service layer checks
// the requested id against the caller's token.

func (r *organizationRepo) Create(ctx context.Context, in domain.CreateOrganizationInput) (*domain.Organization, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	settings, err := settingsToJSON(in.Settings)
	if err != nil {
		return nil, domain.Internal(err)
	}

	var out *domain.Organization
	err = r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.CreateOrganization(ctx, sqlcgen.CreateOrganizationParams{
			Name:     in.Name,
			Slug:     in.Slug,
			Settings: settings,
		})
		if err != nil {
			return translate(err, "organization")
		}
		out, err = orgFromRow(row)
		return err
	})
	return out, err
}

func (r *organizationRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Organization, error) {
	var out *domain.Organization
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetOrganization(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "organization")
		}
		out, err = orgFromRow(row)
		return err
	})
	return out, err
}

func (r *organizationRepo) GetBySlug(ctx context.Context, slug string) (*domain.Organization, error) {
	var out *domain.Organization
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetOrganizationBySlug(ctx, slug)
		if err != nil {
			return translate(err, "organization")
		}
		out, err = orgFromRow(row)
		return err
	})
	return out, err
}

func (r *organizationRepo) List(ctx context.Context, page domain.PageRequest) (domain.Page[*domain.Organization], error) {
	page = page.Normalize()
	var result domain.Page[*domain.Organization]

	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		// One more than asked for, so HasMore is answered without a COUNT.
		rows, err := q.ListOrganizations(ctx, sqlcgen.ListOrganizationsParams{
			CursorCreatedAt: toPgTime(page.Cursor.Time),
			CursorID:        toPgUUID(page.Cursor.ID),
			PageSize:        int32(page.Limit + 1),
		})
		if err != nil {
			return translate(err, "organization")
		}
		orgs := make([]*domain.Organization, 0, len(rows))
		for _, row := range rows {
			o, err := orgFromRow(row)
			if err != nil {
				return err
			}
			orgs = append(orgs, o)
		}
		result = domain.NewPage(orgs, page.Limit, func(o *domain.Organization) domain.Cursor {
			return domain.Cursor{Time: o.CreatedAt, ID: o.ID}
		})
		return nil
	})
	return result, err
}

func (r *organizationRepo) Update(ctx context.Context, id uuid.UUID, in domain.UpdateOrganizationInput) (*domain.Organization, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	var settings []byte
	if in.Settings != nil {
		var err error
		if settings, err = settingsToJSON(*in.Settings); err != nil {
			return nil, domain.Internal(err)
		}
	}
	var status *string
	if in.Status != nil {
		s := string(*in.Status)
		status = &s
	}

	var out *domain.Organization
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.UpdateOrganization(ctx, sqlcgen.UpdateOrganizationParams{
			ID:       toPgUUID(id),
			Name:     in.Name,
			Status:   status,
			Settings: settings,
		})
		if err != nil {
			return translate(err, "organization")
		}
		out, err = orgFromRow(row)
		return err
	})
	return out, err
}

func (r *organizationRepo) SoftDelete(ctx context.Context, id uuid.UUID) error {
	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.SoftDeleteOrganization(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "organization")
		}
		// Zero rows means it was already deleted or never existed. Both are
		// not-found from the caller's point of view; reporting success would
		// tell them they deleted something they did not.
		if n == 0 {
			return notFound("organization", id.String())
		}
		return nil
	})
}

func (r *organizationRepo) CountDevices(ctx context.Context, id uuid.UUID) (int, error) {
	var count int
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.CountDevicesByOrganization(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "organization")
		}
		count = int(n)
		return nil
	})
	return count, err
}

func orgFromRow(row sqlcgen.Organization) (*domain.Organization, error) {
	org := &domain.Organization{
		ID:        fromPgUUID(row.ID),
		Name:      row.Name,
		Slug:      row.Slug,
		Status:    domain.OrgStatus(row.Status),
		CreatedAt: fromPgTime(row.CreatedAt),
		UpdatedAt: fromPgTime(row.UpdatedAt),
		DeletedAt: fromPgTimePtr(row.DeletedAt),
	}
	// Settings carry policy, so a parse failure is surfaced rather than
	// silently defaulted — quietly turning off an approval requirement would be
	// worse than an error.
	if err := settingsFromJSON(row.Settings, &org.Settings); err != nil {
		return nil, domain.Internal(err)
	}
	return org, nil
}
