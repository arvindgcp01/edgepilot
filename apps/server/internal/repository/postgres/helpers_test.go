package postgres_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres"
	"github.com/visionify/edgepilot/apps/server/internal/testsupport"
)

// fixture is a store plus two organizations.
//
// Two, always — a single-tenant fixture cannot demonstrate isolation, and
// isolation is the property most worth testing here. Every test that reads or
// writes gets a neighbour whose data it must not be able to see.
type fixture struct {
	store  *postgres.Store
	acme   *domain.Organization
	globex *domain.Organization
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	db := testsupport.NewMigratedDatabase(t)

	store, err := postgres.New(t.Context(), postgres.Options{
		DatabaseURL: db.DSN,
		MaxConns:    2,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	f := &fixture{store: store}
	f.acme = f.createOrg(t, "Acme", "acme-"+randomSuffix(t))
	f.globex = f.createOrg(t, "Globex", "globex-"+randomSuffix(t))
	return f
}

func (f *fixture) createOrg(t *testing.T, name, slug string) *domain.Organization {
	t.Helper()
	org, err := f.store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: name,
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("create organization %s: %v", slug, err)
	}
	return org
}

// asOrg returns a context scoped to an organization, exactly as the transport
// layer will do from the caller's token.
func (f *fixture) asOrg(t *testing.T, org *domain.Organization) context.Context {
	t.Helper()
	return domain.WithOrg(t.Context(), org.ID)
}

// enrollDevice registers a device in an organization and returns it.
func (f *fixture) enrollDevice(t *testing.T, org *domain.Organization, serial, hostname string) *domain.Device {
	t.Helper()
	dev, err := f.store.Devices().Enroll(f.asOrg(t, org), domain.EnrollDeviceInput{
		DeviceID:     mustUUIDv7(t),
		OrgID:        org.ID,
		SerialNumber: serial,
		Hostname:     hostname,
		Arch:         "arm64",
		OSVersion:    "15.3.1",
	})
	if err != nil {
		t.Fatalf("enroll device %s: %v", serial, err)
	}
	return dev
}

// createUser invites a user with the given roles.
func (f *fixture) createUser(t *testing.T, org *domain.Organization, email string, roles ...string) *domain.User {
	t.Helper()
	if len(roles) == 0 {
		roles = []string{domain.RoleOperator}
	}
	u, err := f.store.Users().Create(f.asOrg(t, org), domain.CreateUserInput{
		OrgID:     org.ID,
		Email:     email,
		FullName:  "Test User",
		RoleSlugs: roles,
	}, "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$hash")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

func mustUUIDv7(t *testing.T) uuid.UUID {
	t.Helper()
	// UUIDv7 rather than v4 so ids sort chronologically and B-tree inserts stay
	// at the right edge of the index instead of scattering across it.
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	return id
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func ptr[T any](v T) *T { return &v }

func timeNow() time.Time  { return time.Now().UTC() }
func timeSoon() time.Time { return time.Now().UTC().Add(24 * time.Hour) }
