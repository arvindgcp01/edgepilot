package domain

import (
	"time"

	"github.com/google/uuid"
)

// OrgStatus is an organization's lifecycle state.
type OrgStatus string

const (
	OrgActive    OrgStatus = "active"
	OrgSuspended OrgStatus = "suspended"
	OrgArchived  OrgStatus = "archived"
)

// Valid reports whether s is a recognised status. Called before a write so a
// typo fails in the service layer with a clear message rather than as a check
// constraint violation from the driver.
func (s OrgStatus) Valid() bool {
	switch s {
	case OrgActive, OrgSuspended, OrgArchived:
		return true
	}
	return false
}

// Organization is a customer, and the tenancy root: every device, user,
// package, and audit record belongs to exactly one.
type Organization struct {
	ID       uuid.UUID
	Name     string
	Slug     string
	Status   OrgStatus
	Settings OrgSettings

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Active reports whether the organization may be used. A suspended or archived
// org keeps its data and its audit trail but accepts no work.
func (o *Organization) Active() bool {
	return o != nil && o.Status == OrgActive && o.DeletedAt == nil
}

// OrgSettings is per-organization policy. It is stored as jsonb rather than
// columns because it is read whole, written whole, and never queried by field —
// and because adding a policy knob should not be a migration.
type OrgSettings struct {
	// RequireApprovalForDestructive forces the two-person rule onto destructive
	// built-in commands, not just custom scripts.
	RequireApprovalForDestructive bool `json:"require_approval_for_destructive,omitempty"`

	// AutoApproveEnrollment lets a device receive commands the moment it
	// enrolls. Off by default: a device that appears unexpectedly should be
	// inert until someone looks at it.
	AutoApproveEnrollment bool `json:"auto_approve_enrollment,omitempty"`

	// MetricsRetentionDays and AuditRetentionDays override the defaults in
	// docs/Database.md §5. Zero means "use the default".
	MetricsRetentionDays int `json:"metrics_retention_days,omitempty"`
	AuditRetentionDays   int `json:"audit_retention_days,omitempty"`
}

// CreateOrganizationInput is what the service layer supplies to create an org.
type CreateOrganizationInput struct {
	Name     string
	Slug     string
	Settings OrgSettings
}

// Validate checks the input before it reaches the database.
//
// The database enforces these too — the slug has a CHECK constraint, the name a
// length bound. Validating here as well is not redundant: it produces a field-
// level error the caller can act on instead of a driver error nobody can map to
// a form field.
func (in CreateOrganizationInput) Validate() error {
	var fields []FieldError
	if l := len(in.Name); l < 1 || l > 200 {
		fields = append(fields, FieldError{Pointer: "/name", Message: "must be 1 to 200 characters"})
	}
	if !validSlug(in.Slug) {
		fields = append(fields, FieldError{
			Pointer: "/slug",
			Message: "must be 2 to 63 characters of lowercase letters, digits, and hyphens, starting with a letter or digit",
		})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// UpdateOrganizationInput carries a partial update. A nil field is left alone,
// which is what distinguishes "not supplied" from "set to empty".
type UpdateOrganizationInput struct {
	Name     *string
	Status   *OrgStatus
	Settings *OrgSettings
}

func (in UpdateOrganizationInput) Validate() error {
	var fields []FieldError
	if in.Name != nil {
		if l := len(*in.Name); l < 1 || l > 200 {
			fields = append(fields, FieldError{Pointer: "/name", Message: "must be 1 to 200 characters"})
		}
	}
	if in.Status != nil && !in.Status.Valid() {
		fields = append(fields, FieldError{
			Pointer: "/status",
			Message: "must be one of: active, suspended, archived",
		})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// validSlug mirrors the CHECK constraint on organizations.slug:
// ^[a-z0-9][a-z0-9-]{1,62}$
func validSlug(s string) bool {
	if len(s) < 2 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}
