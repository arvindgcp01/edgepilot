package postgres_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// Tenant isolation, exercised through the repository API rather than raw SQL.
//
// The schema tests in migrations/testdata already prove that PostgreSQL's
// row-level security policies work. These prove something different and equally
// necessary: that the repository layer actually *engages* those policies on
// every path. A correct policy set reached through a query that forgot to scope
// its transaction protects nothing.

func TestDeviceIsInvisibleToAnotherTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeDevice := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	// A direct lookup by primary key, from the wrong organization. This is the
	// attack: the caller knows the id and asks for it anyway.
	_, err := f.store.Devices().Get(f.asOrg(t, f.globex), acmeDevice.ID)
	if err == nil {
		t.Fatal("a device was readable from another organization")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant read should be indistinguishable from absent, got: %v", err)
	}

	// And the owner can still read it, so the test is not passing because the
	// device simply does not exist.
	got, err := f.store.Devices().Get(f.asOrg(t, f.acme), acmeDevice.ID)
	if err != nil {
		t.Fatalf("owner could not read its own device: %v", err)
	}
	if got.ID != acmeDevice.ID {
		t.Errorf("wrong device returned: %s", got.ID)
	}
}

// The response for another tenant's resource must be identical to the response
// for one that does not exist. Anything else — a different code, a different
// message — turns the API into an oracle for enumerating another customer's
// fleet.
func TestCrossTenantAndAbsentAreIndistinguishable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeDevice := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	_, crossTenant := f.store.Devices().Get(f.asOrg(t, f.globex), acmeDevice.ID)
	_, absent := f.store.Devices().Get(f.asOrg(t, f.globex), mustUUIDv7(t))

	if crossTenant == nil || absent == nil {
		t.Fatal("both lookups should fail")
	}
	if domain.KindOf(crossTenant) != domain.KindOf(absent) {
		t.Errorf("error kinds differ: %v vs %v", domain.KindOf(crossTenant), domain.KindOf(absent))
	}
	if domain.CodeOf(crossTenant) != domain.CodeOf(absent) {
		t.Errorf("error codes differ, disclosing existence: %q vs %q",
			domain.CodeOf(crossTenant), domain.CodeOf(absent))
	}
}

func TestListingNeverCrossesTenants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")
	f.enrollDevice(t, f.acme, "ACME-2", "acme-mac-2")
	f.enrollDevice(t, f.globex, "GLOBEX-1", "globex-mac-1")

	acmePage, err := f.store.Devices().List(f.asOrg(t, f.acme), domain.DeviceFilter{}, domain.PageRequest{})
	if err != nil {
		t.Fatalf("list acme devices: %v", err)
	}
	if len(acmePage.Items) != 2 {
		t.Errorf("acme sees %d devices, want 2", len(acmePage.Items))
	}
	for _, d := range acmePage.Items {
		if d.OrgID != f.acme.ID {
			t.Errorf("acme's listing contains a device from org %s", d.OrgID)
		}
	}

	globexPage, err := f.store.Devices().List(f.asOrg(t, f.globex), domain.DeviceFilter{}, domain.PageRequest{})
	if err != nil {
		t.Fatalf("list globex devices: %v", err)
	}
	if len(globexPage.Items) != 1 {
		t.Errorf("globex sees %d devices, want 1", len(globexPage.Items))
	}
}

// Writes are as important as reads: updating another tenant's device must fail
// even though the caller supplied a valid id.
func TestWritesCannotReachAnotherTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeDevice := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	_, err := f.store.Devices().Update(f.asOrg(t, f.globex), acmeDevice.ID, domain.UpdateDeviceInput{
		Notes: ptr("owned"),
	})
	if err == nil {
		t.Fatal("another organization updated a device it does not own")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected not-found, got: %v", err)
	}

	// The device must be unchanged.
	got, err := f.store.Devices().Get(f.asOrg(t, f.acme), acmeDevice.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Notes != "" {
		t.Errorf("notes were modified across a tenant boundary: %q", got.Notes)
	}
}

func TestDecommissionCannotReachAnotherTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeDevice := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	if err := f.store.Devices().Decommission(f.asOrg(t, f.globex), acmeDevice.ID); err == nil {
		t.Fatal("another organization decommissioned a device it does not own")
	}

	got, err := f.store.Devices().Get(f.asOrg(t, f.acme), acmeDevice.ID)
	if err != nil {
		t.Fatalf("device should still exist: %v", err)
	}
	if got.DeletedAt != nil {
		t.Error("device was deleted across a tenant boundary")
	}
}

func TestUsersAreIsolatedByTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeUser := f.createUser(t, f.acme, "someone@acme.test")
	f.createUser(t, f.globex, "someone@globex.test")

	if _, err := f.store.Users().Get(f.asOrg(t, f.globex), acmeUser.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a user was readable from another organization: %v", err)
	}

	page, err := f.store.Users().List(f.asOrg(t, f.globex), domain.PageRequest{})
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range page.Items {
		if u.OrgID != f.globex.ID {
			t.Errorf("listing leaked a user from org %s", u.OrgID)
		}
	}
}

// The same email may exist in two organizations. It is not a global identifier,
// and treating it as one would let one customer's signup block another's.
func TestSameEmailMayExistInDifferentTenants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const email = "ops@shared.example"
	f.createUser(t, f.acme, email)

	if _, err := f.store.Users().Create(f.asOrg(t, f.globex), domain.CreateUserInput{
		OrgID:     f.globex.ID,
		Email:     email,
		FullName:  "Other Org",
		RoleSlugs: []string{domain.RoleViewer},
	}, "hash"); err != nil {
		t.Fatalf("the same email should be usable in a different organization: %v", err)
	}

	// But not twice within one.
	_, err := f.store.Users().Create(f.asOrg(t, f.acme), domain.CreateUserInput{
		OrgID:     f.acme.ID,
		Email:     email,
		FullName:  "Duplicate",
		RoleSlugs: []string{domain.RoleViewer},
	}, "hash")
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a duplicate email within one organization should conflict, got: %v", err)
	}
	if code := domain.CodeOf(err); code != "email_already_in_use" {
		t.Errorf("conflict code = %q, want email_already_in_use", code)
	}
}

// Enrollment tokens establish which organization a device joins, so a token
// leaking across tenants would let a device be enrolled into the wrong fleet.
func TestEnrollmentTokensAreIsolatedByTenant(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.store.EnrollmentTokens().Create(f.asOrg(t, f.acme),
		domain.CreateEnrollmentTokenInput{
			OrgID:       f.acme.ID,
			Description: "acme site 1",
			ExpiresAt:   timeSoon(),
		}, hashToken("acme-token"), "ep_enroll_acme")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	page, err := f.store.EnrollmentTokens().List(f.asOrg(t, f.globex), domain.PageRequest{})
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("globex sees %d of acme's enrollment tokens", len(page.Items))
	}
}

// Certificates hang off devices rather than carrying org_id themselves, so
// their isolation depends on the EXISTS-on-parent policy. That is a different
// mechanism from the direct org_id policies and needs its own test.
func TestDeviceCertificatesAreIsolatedThroughTheirDevice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	acmeDevice := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	cert := &domain.DeviceCertificate{
		DeviceID:          acmeDevice.ID,
		Serial:            "1234567890123456789012345",
		FingerprintSHA256: hashToken("acme-cert"),
		PublicKeyDER:      []byte{0x30, 0x2a},
		CertificatePEM:    "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		NotBefore:         timeNow(),
		NotAfter:          timeSoon(),
		IssuedVia:         "enrollment",
	}
	if _, err := f.store.DeviceCertificates().Issue(f.asOrg(t, f.acme), cert); err != nil {
		t.Fatalf("issue certificate: %v", err)
	}

	certs, err := f.store.DeviceCertificates().ListForDevice(f.asOrg(t, f.globex), acmeDevice.ID)
	if err != nil {
		t.Fatalf("list certificates: %v", err)
	}
	if len(certs) != 0 {
		t.Errorf("globex sees %d of acme's device certificates", len(certs))
	}

	owner, err := f.store.DeviceCertificates().ListForDevice(f.asOrg(t, f.acme), acmeDevice.ID)
	if err != nil {
		t.Fatalf("owner list: %v", err)
	}
	if len(owner) != 1 {
		t.Errorf("owner sees %d certificates, want 1", len(owner))
	}
}

// Using a scoped repository without a scope is a programming error, and it must
// fail loudly. Returning an empty result would look like "no data" and hide the
// missing scope until someone noticed a page was mysteriously blank.
func TestUnscopedContextIsRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	// t.Context() carries no organization.
	_, err := f.store.Devices().List(t.Context(), domain.DeviceFilter{}, domain.PageRequest{})
	if err == nil {
		t.Fatal("a scoped repository must refuse an unscoped context, not return an empty page")
	}
	if domain.KindOf(err) != domain.KindInternal {
		t.Errorf("missing scope is a programming error and should be internal, got %v", domain.KindOf(err))
	}

	if _, err := f.store.Devices().Get(t.Context(), uuid.New()); err == nil {
		t.Error("Get must also refuse an unscoped context")
	}
}
