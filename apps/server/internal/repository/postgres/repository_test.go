package postgres_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// --- organizations ---------------------------------------------------------

func TestOrganizationCRUD(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	got, err := f.store.Organizations().Get(ctx, f.acme.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Acme" || got.Status != domain.OrgActive {
		t.Errorf("unexpected organization: %+v", got)
	}

	bySlug, err := f.store.Organizations().GetBySlug(ctx, f.acme.Slug)
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if bySlug.ID != f.acme.ID {
		t.Error("slug lookup returned the wrong organization")
	}

	// Settings round-trip through jsonb without losing a field.
	updated, err := f.store.Organizations().Update(ctx, f.acme.ID, domain.UpdateOrganizationInput{
		Name: ptr("Acme Corporation"),
		Settings: &domain.OrgSettings{
			RequireApprovalForDestructive: true,
			MetricsRetentionDays:          120,
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Acme Corporation" {
		t.Errorf("name = %q", updated.Name)
	}
	if !updated.Settings.RequireApprovalForDestructive || updated.Settings.MetricsRetentionDays != 120 {
		t.Errorf("settings did not round-trip: %+v", updated.Settings)
	}

	// A partial update must leave unmentioned fields alone.
	partial, err := f.store.Organizations().Update(ctx, f.acme.ID, domain.UpdateOrganizationInput{
		Status: ptr(domain.OrgSuspended),
	})
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	if partial.Name != "Acme Corporation" {
		t.Errorf("a partial update clobbered the name: %q", partial.Name)
	}
	if !partial.Settings.RequireApprovalForDestructive {
		t.Error("a partial update clobbered the settings")
	}
	if partial.Active() {
		t.Error("a suspended organization must not report active")
	}
}

func TestOrganizationValidationRejectsBadInput(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for _, tc := range []struct {
		name string
		in   domain.CreateOrganizationInput
	}{
		{"empty name", domain.CreateOrganizationInput{Name: "", Slug: "valid-slug"}},
		{"empty slug", domain.CreateOrganizationInput{Name: "Valid", Slug: ""}},
		{"uppercase slug", domain.CreateOrganizationInput{Name: "Valid", Slug: "Not-Lower"}},
		{"slug with spaces", domain.CreateOrganizationInput{Name: "Valid", Slug: "has spaces"}},
		{"slug starting with hyphen", domain.CreateOrganizationInput{Name: "Valid", Slug: "-leading"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.store.Organizations().Create(t.Context(), tc.in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("expected a validation error, got: %v", err)
			}
		})
	}
}

func TestOrganizationSlugConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.store.Organizations().Create(t.Context(), domain.CreateOrganizationInput{
		Name: "Duplicate",
		Slug: f.acme.Slug,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected a conflict, got: %v", err)
	}
	if code := domain.CodeOf(err); code != "slug_already_in_use" {
		t.Errorf("code = %q, want slug_already_in_use", code)
	}
}

func TestOrganizationSoftDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	org := f.createOrg(t, "Temporary", "temp-"+randomSuffix(t))
	if err := f.store.Organizations().SoftDelete(ctx, org.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := f.store.Organizations().Get(ctx, org.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a soft-deleted organization should read as absent, got: %v", err)
	}
	// Deleting twice is not-found, not a silent success that implies something
	// happened.
	if err := f.store.Organizations().SoftDelete(ctx, org.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete should report not-found, got: %v", err)
	}
}

// --- devices ---------------------------------------------------------------

func TestDeviceEnrollmentIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	first, err := f.store.Devices().Enroll(ctx, domain.EnrollDeviceInput{
		DeviceID:     mustUUIDv7(t),
		OrgID:        f.acme.ID,
		SerialNumber: "C02XY1234ABC",
		Hostname:     "mac-1",
		OSVersion:    "15.3.0",
		Arch:         "arm64",
	})
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}

	// An agent retrying after a network failure sends the same serial again.
	// It must reclaim its record, not collide and not duplicate.
	second, err := f.store.Devices().Enroll(ctx, domain.EnrollDeviceInput{
		DeviceID:     mustUUIDv7(t),
		OrgID:        f.acme.ID,
		SerialNumber: "C02XY1234ABC",
		Hostname:     "mac-1-renamed",
		OSVersion:    "15.4.0",
		Arch:         "arm64",
	})
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-enrollment created a second record: %s vs %s", second.ID, first.ID)
	}
	if second.Hostname != "mac-1-renamed" {
		t.Errorf("hostname was not refreshed: %q", second.Hostname)
	}
	if second.OSVersion != "15.4.0" {
		t.Errorf("os version was not refreshed: %q", second.OSVersion)
	}

	page, err := f.store.Devices().List(ctx, domain.DeviceFilter{}, domain.PageRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Errorf("expected exactly 1 device after re-enrollment, got %d", len(page.Items))
	}
}

// An approved device must not silently return to pending because someone
// reinstalled the agent, and a rejected one must not become pending either.
func TestReEnrollmentPreservesEnrollmentState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	dev := f.enrollDevice(t, f.acme, "C02XY1234ABC", "mac-1")
	if _, err := f.store.Devices().SetEnrollmentState(ctx, dev.ID, domain.EnrollmentApproved); err != nil {
		t.Fatalf("approve: %v", err)
	}

	again, err := f.store.Devices().Enroll(ctx, domain.EnrollDeviceInput{
		DeviceID:     mustUUIDv7(t),
		OrgID:        f.acme.ID,
		SerialNumber: "C02XY1234ABC",
		Hostname:     "mac-1",
	})
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if again.EnrollmentState != domain.EnrollmentApproved {
		t.Errorf("re-enrollment reset the enrollment state to %q", again.EnrollmentState)
	}
	if !again.Approved() {
		t.Error("device should still be approved")
	}
}

func TestDeviceEnrollmentAutoApprove(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	dev, err := f.store.Devices().Enroll(ctx, domain.EnrollDeviceInput{
		DeviceID:     mustUUIDv7(t),
		OrgID:        f.acme.ID,
		SerialNumber: "AUTO-1",
		Hostname:     "auto",
		AutoApprove:  true,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if dev.EnrollmentState != domain.EnrollmentApproved {
		t.Errorf("auto-approve did not take effect: %q", dev.EnrollmentState)
	}

	// Without it, a device is inert until an admin looks at it.
	plain := f.enrollDevice(t, f.acme, "MANUAL-1", "manual")
	if plain.EnrollmentState != domain.EnrollmentPending {
		t.Errorf("default enrollment state = %q, want pending", plain.EnrollmentState)
	}
	if plain.Approved() {
		t.Error("a pending device must not report approved: it receives no commands")
	}
}

func TestDeviceUpdateTouchesOnlyOperatorFields(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	dev := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	updated, err := f.store.Devices().Update(ctx, dev.ID, domain.UpdateDeviceInput{
		Site:               ptr("Warehouse 3"),
		Notes:              ptr("behind the router"),
		Labels:             map[string]string{"ring": "canary", "site": "wh3"},
		AllowCustomScripts: ptr(true),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Site != "Warehouse 3" || updated.Notes != "behind the router" {
		t.Errorf("operator fields not applied: %+v", updated)
	}
	if updated.Labels["ring"] != "canary" {
		t.Errorf("labels not applied: %v", updated.Labels)
	}
	if !updated.AllowCustomScripts {
		t.Error("allow_custom_scripts not applied")
	}
	// Hardware facts come from inventory and are not settable through this path.
	if updated.SerialNumber != dev.SerialNumber || updated.Arch != dev.Arch {
		t.Error("an operator update changed hardware facts")
	}
}

func TestDeviceLabelValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)
	dev := f.enrollDevice(t, f.acme, "ACME-1", "acme-mac-1")

	tooMany := map[string]string{}
	for i := range 40 {
		tooMany[fmt.Sprintf("key%d", i)] = "v"
	}

	for name, labels := range map[string]map[string]string{
		"too many labels": tooMany,
		"empty key":       {"": "v"},
		"oversized value": {"k": string(make([]byte, 300))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := f.store.Devices().Update(ctx, dev.ID, domain.UpdateDeviceInput{Labels: labels})
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("expected validation error, got: %v", err)
			}
		})
	}
}

func TestDeviceFilteringAndSearch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	a := f.enrollDevice(t, f.acme, "SERIAL-AAA", "warehouse-mac")
	b := f.enrollDevice(t, f.acme, "SERIAL-BBB", "office-mac")
	f.enrollDevice(t, f.acme, "SERIAL-CCC", "lab-mac")

	if _, err := f.store.Devices().Update(ctx, a.ID, domain.UpdateDeviceInput{
		Labels: map[string]string{"ring": "canary", "site": "wh"},
	}); err != nil {
		t.Fatalf("label a: %v", err)
	}
	if _, err := f.store.Devices().Update(ctx, b.ID, domain.UpdateDeviceInput{
		Labels: map[string]string{"ring": "stable"},
	}); err != nil {
		t.Fatalf("label b: %v", err)
	}
	if _, err := f.store.Devices().SetEnrollmentState(ctx, a.ID, domain.EnrollmentApproved); err != nil {
		t.Fatalf("approve a: %v", err)
	}

	t.Run("search matches hostname", func(t *testing.T) {
		page, err := f.store.Devices().List(ctx, domain.DeviceFilter{Search: "warehouse"}, domain.PageRequest{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != a.ID {
			t.Errorf("search returned %d devices", len(page.Items))
		}
	})

	t.Run("search matches partial serial", func(t *testing.T) {
		page, err := f.store.Devices().List(ctx, domain.DeviceFilter{Search: "BBB"}, domain.PageRequest{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != b.ID {
			t.Errorf("serial search returned %d devices", len(page.Items))
		}
	})

	// A search term containing SQL LIKE wildcards must be treated literally,
	// not as "match everything".
	t.Run("wildcards in the search term are escaped", func(t *testing.T) {
		page, err := f.store.Devices().List(ctx, domain.DeviceFilter{Search: "%"}, domain.PageRequest{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 0 {
			t.Errorf("a literal %% matched %d devices; wildcards are not being escaped", len(page.Items))
		}
	})

	t.Run("label selector ANDs its pairs", func(t *testing.T) {
		page, err := f.store.Devices().List(ctx,
			domain.DeviceFilter{Labels: map[string]string{"ring": "canary"}}, domain.PageRequest{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != a.ID {
			t.Errorf("label filter returned %d devices", len(page.Items))
		}

		// Both pairs must match, not either.
		page, err = f.store.Devices().List(ctx,
			domain.DeviceFilter{Labels: map[string]string{"ring": "canary", "site": "nowhere"}},
			domain.PageRequest{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 0 {
			t.Errorf("label filter is ORing rather than ANDing: matched %d", len(page.Items))
		}
	})

	t.Run("enrollment state filter", func(t *testing.T) {
		approved := domain.EnrollmentApproved
		page, err := f.store.Devices().List(ctx,
			domain.DeviceFilter{EnrollmentState: &approved}, domain.PageRequest{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != a.ID {
			t.Errorf("enrollment filter returned %d devices", len(page.Items))
		}
	})

	t.Run("unknown sort is rejected", func(t *testing.T) {
		_, err := f.store.Devices().List(ctx,
			domain.DeviceFilter{Sort: "; DROP TABLE devices --"}, domain.PageRequest{})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("an unknown sort must be rejected, got: %v", err)
		}
	})
}

// Cursor pagination must visit every row exactly once. That is the whole
// argument for using it over OFFSET, so it is worth proving rather than
// assuming.
func TestDevicePaginationVisitsEveryRowOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	const total = 25
	for i := range total {
		f.enrollDevice(t, f.acme, fmt.Sprintf("SERIAL-%03d", i), fmt.Sprintf("mac-%03d", i))
	}

	for _, sort := range []domain.DeviceSort{
		domain.DeviceSortCreatedDesc,
		domain.DeviceSortCreatedAsc,
		domain.DeviceSortHostnameAsc,
		domain.DeviceSortHostnameDesc,
	} {
		t.Run(string(sort), func(t *testing.T) {
			seen := map[string]int{}
			pages := 0
			req := domain.PageRequest{Limit: 7}

			for {
				page, err := f.store.Devices().List(ctx, domain.DeviceFilter{Sort: sort}, req)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				for _, d := range page.Items {
					seen[d.ID.String()]++
				}
				pages++
				if pages > 20 {
					t.Fatal("pagination did not terminate")
				}
				if !page.HasMore {
					break
				}
				cursor, err := domain.DecodeCursor(page.NextCursor)
				if err != nil {
					t.Fatalf("decode cursor: %v", err)
				}
				req.Cursor = cursor
			}

			if len(seen) != total {
				t.Errorf("saw %d distinct devices across %d pages, want %d", len(seen), pages, total)
			}
			for id, n := range seen {
				if n != 1 {
					t.Errorf("device %s appeared %d times", id, n)
				}
			}
		})
	}
}

func TestDeviceSummaryCounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	a := f.enrollDevice(t, f.acme, "S-1", "mac-1")
	f.enrollDevice(t, f.acme, "S-2", "mac-2")
	// Another tenant's devices must not appear in the counters.
	f.enrollDevice(t, f.globex, "G-1", "globex-1")

	if _, err := f.store.Devices().SetEnrollmentState(ctx, a.ID, domain.EnrollmentApproved); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := f.store.Devices().SetStatus(ctx, a.ID, domain.DeviceStatusOnline); err != nil {
		t.Fatalf("set status: %v", err)
	}

	summary, err := f.store.Devices().Summary(ctx, 90*time.Second)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Total != 2 {
		t.Errorf("total = %d, want 2 (another tenant's devices leaked in)", summary.Total)
	}
	if summary.Online != 1 {
		t.Errorf("online = %d, want 1", summary.Online)
	}
	if summary.PendingApproval != 1 {
		t.Errorf("pending = %d, want 1", summary.PendingApproval)
	}
}

func TestTouchLastSeenIsBatchedAndMonotonic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	a := f.enrollDevice(t, f.acme, "S-1", "mac-1")
	b := f.enrollDevice(t, f.acme, "S-2", "mac-2")

	now := time.Now().UTC().Truncate(time.Millisecond)
	batch := map[uuid.UUID]time.Time{a.ID: now, b.ID: now}
	if err := f.store.Devices().TouchLastSeen(t.Context(), batch); err != nil {
		t.Fatalf("touch: %v", err)
	}

	got, err := f.store.Devices().Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastSeenAt == nil {
		t.Fatal("last_seen_at was not set")
	}
	if !got.LastSeenAt.Equal(now) {
		t.Errorf("last_seen_at = %v, want %v", got.LastSeenAt, now)
	}

	// A late flush from a slow replica must not drag a live device backwards.
	earlier := now.Add(-time.Minute)
	if err := f.store.Devices().TouchLastSeen(t.Context(),
		map[uuid.UUID]time.Time{a.ID: earlier, b.ID: earlier}); err != nil {
		t.Fatalf("touch with older timestamp: %v", err)
	}
	got, err = f.store.Devices().Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LastSeenAt.Equal(now) {
		t.Errorf("a stale flush moved last_seen_at backwards to %v", got.LastSeenAt)
	}

	// An empty batch is a no-op, not an error: a flush interval with no
	// heartbeats is normal.
	if err := f.store.Devices().TouchLastSeen(t.Context(), nil); err != nil {
		t.Errorf("empty batch should be a no-op: %v", err)
	}
}

// --- enrollment tokens -----------------------------------------------------

func TestEnrollmentTokenRedeemIsAtomic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const secret = "ep_enroll_single_use"
	_, err := f.store.EnrollmentTokens().Create(f.asOrg(t, f.acme),
		domain.CreateEnrollmentTokenInput{
			OrgID:       f.acme.ID,
			Description: "single use",
			MaxUses:     ptr(1),
			ExpiresAt:   timeSoon(),
		}, hashToken(secret), "ep_enroll_sing")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// Two agents enrolling at the same instant against a single-use token.
	// Exactly one must win; a check-then-update would let both through.
	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.store.EnrollmentTokens().Redeem(t.Context(), hashToken(secret), time.Now()); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("%d of %d concurrent redemptions succeeded against a single-use token, want exactly 1",
			successes, racers)
	}
}

func TestEnrollmentTokenRejectsUnusableTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	t.Run("expired", func(t *testing.T) {
		const secret = "ep_enroll_expired"
		// Created with a future expiry — validation rejects a past one — then
		// redeemed at a later moment.
		if _, err := f.store.EnrollmentTokens().Create(ctx, domain.CreateEnrollmentTokenInput{
			OrgID:     f.acme.ID,
			ExpiresAt: time.Now().Add(time.Second),
		}, hashToken(secret), "ep_enroll_exp"); err != nil {
			t.Fatalf("create: %v", err)
		}
		// The caller's context is passed through even though Redeem runs
		// unscoped, so cancellation and deadlines propagate as they will in
		// production. Redeem ignores the tenant scope, not the context.
		_, err := f.store.EnrollmentTokens().Redeem(ctx, hashToken(secret), time.Now().Add(time.Hour))
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("an expired token should be unusable, got: %v", err)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		const secret = "ep_enroll_revoked"
		tok, err := f.store.EnrollmentTokens().Create(ctx, domain.CreateEnrollmentTokenInput{
			OrgID:     f.acme.ID,
			ExpiresAt: timeSoon(),
		}, hashToken(secret), "ep_enroll_rev")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := f.store.EnrollmentTokens().Revoke(ctx, tok.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := f.store.EnrollmentTokens().Redeem(ctx, hashToken(secret), time.Now()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("a revoked token should be unusable, got: %v", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		_, err := f.store.EnrollmentTokens().Redeem(ctx, hashToken("never-created"), time.Now())
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("an unknown token should be unusable, got: %v", err)
		}
	})

	// Past expiry is rejected at creation, so a token cannot be born dead.
	t.Run("cannot create with a past expiry", func(t *testing.T) {
		_, err := f.store.EnrollmentTokens().Create(ctx, domain.CreateEnrollmentTokenInput{
			OrgID:     f.acme.ID,
			ExpiresAt: time.Now().Add(-time.Hour),
		}, hashToken("past"), "ep_enroll_past")
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("expected validation error, got: %v", err)
		}
	})
}

// --- users, roles, sessions ------------------------------------------------

func TestUserRolesAndPermissionResolution(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	user := f.createUser(t, f.acme, "operator@acme.test", domain.RoleOperator)

	roles, perms, err := f.store.Users().ResolvePermissions(ctx, user.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(roles) != 1 || roles[0] != domain.RoleOperator {
		t.Errorf("roles = %v, want [operator]", roles)
	}

	held := map[string]bool{}
	for _, p := range perms {
		held[p] = true
	}
	// Operator can run commands...
	for _, p := range []string{domain.PermDeviceRead, domain.PermCommandExecute, domain.PermSSHConnect} {
		if !held[p] {
			t.Errorf("operator should hold %s", p)
		}
	}
	// ...but must not be able to request a custom script or approve one. If one
	// role could do both, the two-person rule would be theatre.
	for _, p := range []string{domain.PermCommandCustomScript, domain.PermCommandApprove, domain.PermUserWrite} {
		if held[p] {
			t.Errorf("operator must not hold %s", p)
		}
	}
}

func TestReplaceUserRoles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	user := f.createUser(t, f.acme, "promote@acme.test", domain.RoleViewer)

	if err := f.store.Roles().ReplaceUserRoles(ctx, user.ID, []string{domain.RoleAdmin}, nil); err != nil {
		t.Fatalf("replace roles: %v", err)
	}
	roles, perms, err := f.store.Users().ResolvePermissions(ctx, user.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(roles) != 1 || roles[0] != domain.RoleAdmin {
		t.Errorf("roles = %v, want [admin]", roles)
	}
	held := map[string]bool{}
	for _, p := range perms {
		held[p] = true
	}
	if !held[domain.PermUserWrite] {
		t.Error("admin should hold user:write")
	}
	// agent:release is a vendor capability, not a customer-admin one.
	if held[domain.PermAgentRelse] {
		t.Error("a customer admin must not hold agent:release")
	}

	// An unknown role must be reported, not silently ignored — a user left with
	// fewer permissions than requested and no explanation is worse than an error.
	if err := f.store.Roles().ReplaceUserRoles(ctx, user.ID, []string{"wizard"}, nil); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("unknown role should be rejected, got: %v", err)
	}
	if err := f.store.Roles().ReplaceUserRoles(ctx, user.ID, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("empty role list should be rejected, got: %v", err)
	}
}

func TestDisablingUserRevokesSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	user := f.createUser(t, f.acme, "leaver@acme.test")

	family := mustUUIDv7(t)
	session, err := f.store.Sessions().Create(t.Context(), &domain.Session{
		UserID:    user.ID,
		OrgID:     f.acme.ID,
		FamilyID:  family,
		UserAgent: "test",
		ExpiresAt: timeSoon(),
	}, hashToken("refresh-1"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !session.Active(time.Now()) {
		t.Fatal("new session should be active")
	}

	if _, err := f.store.Users().Update(ctx, user.ID, domain.UpdateUserInput{
		Status: ptr(domain.UserDisabled),
	}); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	// A disabled account whose sessions stayed live would keep working for the
	// whole refresh-token lifetime.
	got, err := f.store.Sessions().GetByTokenHash(t.Context(), hashToken("refresh-1"))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Active(time.Now()) {
		t.Error("disabling a user must revoke their sessions in the same transaction")
	}
}

func TestSessionRotationDetectsReuse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	user := f.createUser(t, f.acme, "rotate@acme.test")
	family := mustUUIDv7(t)

	first, err := f.store.Sessions().Create(t.Context(), &domain.Session{
		UserID: user.ID, OrgID: f.acme.ID, FamilyID: family, ExpiresAt: timeSoon(),
	}, hashToken("token-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	second, err := f.store.Sessions().Rotate(t.Context(), first.ID, &domain.Session{
		UserID: user.ID, OrgID: f.acme.ID, FamilyID: family, ExpiresAt: timeSoon(),
	}, hashToken("token-2"))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if second.ParentID == nil || *second.ParentID != first.ID {
		t.Error("the successor should record its parent, for the rotation chain")
	}

	// Presenting the original token again means it leaked. Rotating it a second
	// time must fail rather than mint another live session.
	_, err = f.store.Sessions().Rotate(t.Context(), first.ID, &domain.Session{
		UserID: user.ID, OrgID: f.acme.ID, FamilyID: family, ExpiresAt: timeSoon(),
	}, hashToken("token-3"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reusing a rotated token must be refused, got: %v", err)
	}

	// The response to reuse is to end the whole family: we cannot tell which
	// holder is legitimate.
	n, err := f.store.Sessions().RevokeFamily(t.Context(), family, "refresh token reuse detected")
	if err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	if n != 1 {
		t.Errorf("revoked %d sessions, want 1 (the successor; the original was already revoked)", n)
	}
	got, err := f.store.Sessions().GetByTokenHash(t.Context(), hashToken("token-2"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Active(time.Now()) {
		t.Error("the successor should be revoked after family revocation")
	}
}

func TestLoginFailureLocksAtThreshold(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	user := f.createUser(t, f.acme, "lockme@acme.test")

	const threshold = 3
	lockUntil := time.Now().UTC().Add(15 * time.Minute)
	for attempt := 1; attempt <= threshold; attempt++ {
		locked, err := f.store.Users().RecordLoginFailure(t.Context(), user.ID, lockUntil, threshold)
		if err != nil {
			t.Fatalf("record failure %d: %v", attempt, err)
		}
		if attempt < threshold && locked {
			t.Errorf("locked early, after %d of %d attempts", attempt, threshold)
		}
		if attempt == threshold && !locked {
			t.Errorf("not locked after reaching the threshold of %d", threshold)
		}
	}

	creds, err := f.store.Users().Credentials(t.Context(), f.acme.ID, "lockme@acme.test")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.LockedUntil == nil || !creds.LockedUntil.After(time.Now()) {
		t.Error("locked_until should be in the future")
	}

	// A locked account must not authenticate even with the right password.
	u, err := f.store.Users().Get(ctx, user.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if u.CanAuthenticate(time.Now()) {
		t.Error("a locked account must not be able to authenticate")
	}

	// Success clears the counter and the lock.
	if err := f.store.Users().RecordLoginSuccess(t.Context(), user.ID, time.Now()); err != nil {
		t.Fatalf("record success: %v", err)
	}
	creds, err = f.store.Users().Credentials(t.Context(), f.acme.ID, "lockme@acme.test")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.FailedLoginCount != 0 || creds.LockedUntil != nil {
		t.Errorf("a successful login should clear the lock: count=%d until=%v",
			creds.FailedLoginCount, creds.LockedUntil)
	}
}

// Credentials must be loadable for an account that cannot sign in.
//
// Short-circuiting on status before the password comparison would make a
// disabled account respond faster than an active one, and that timing
// difference discloses which accounts exist.
func TestCredentialsLoadEvenForUnusableAccounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	user := f.createUser(t, f.acme, "disabled@acme.test")
	if _, err := f.store.Users().Update(ctx, user.ID, domain.UpdateUserInput{
		Status: ptr(domain.UserDisabled),
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	creds, err := f.store.Users().Credentials(t.Context(), f.acme.ID, "disabled@acme.test")
	if err != nil {
		t.Fatalf("credentials for a disabled account must still load: %v", err)
	}
	if creds.PasswordHash == "" {
		t.Error("password hash should be present so the comparison still runs")
	}
	if creds.Status != domain.UserDisabled {
		t.Errorf("status = %q, want disabled", creds.Status)
	}
}

// The User entity has no password field, so a list endpoint cannot leak one.
func TestUserEntityCarriesNoSecrets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	f.createUser(t, f.acme, "listed@acme.test")

	page, err := f.store.Users().List(ctx, domain.PageRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("expected at least one user")
	}
	// Compile-time, effectively: domain.User has no hash or MFA-secret field.
	// This asserts the rendered form carries nothing secret either.
	rendered := fmt.Sprintf("%+v", page.Items[0])
	for _, leaked := range []string{"argon2id", "$m=65536", "hash"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("user entity rendered secret material %q: %s", leaked, rendered)
		}
	}
}

// --- transactions ----------------------------------------------------------

// A multi-repository operation must be all-or-nothing. Enrolling a device and
// issuing its certificate is the motivating case: a device with no certificate
// can never connect, and a certificate with no device is unreachable.
func TestInTxRollsBackEverythingOnFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	sentinel := errors.New("deliberate failure")
	deviceID := mustUUIDv7(t)

	err := f.store.InTx(ctx, func(tx domain.Store) error {
		if _, err := tx.Devices().Enroll(ctx, domain.EnrollDeviceInput{
			DeviceID:     deviceID,
			OrgID:        f.acme.ID,
			SerialNumber: "TX-1",
			Hostname:     "tx-mac",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx should surface the callback's error, got: %v", err)
	}

	if _, err := f.store.Devices().Get(ctx, deviceID); !errors.Is(err, domain.ErrNotFound) {
		t.Error("the device survived a rolled-back transaction")
	}
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	deviceID := mustUUIDv7(t)
	err := f.store.InTx(ctx, func(tx domain.Store) error {
		dev, err := tx.Devices().Enroll(ctx, domain.EnrollDeviceInput{
			DeviceID:     deviceID,
			OrgID:        f.acme.ID,
			SerialNumber: "TX-2",
			Hostname:     "tx-mac-2",
		})
		if err != nil {
			return err
		}
		_, err = tx.DeviceCertificates().Issue(ctx, &domain.DeviceCertificate{
			DeviceID:          dev.ID,
			Serial:            "987654321098765432109",
			FingerprintSHA256: hashToken("tx-cert"),
			PublicKeyDER:      []byte{0x30},
			CertificatePEM:    "PEM",
			NotBefore:         timeNow(),
			NotAfter:          timeSoon(),
			IssuedVia:         "enrollment",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if _, err := f.store.Devices().Get(ctx, deviceID); err != nil {
		t.Errorf("device should exist after commit: %v", err)
	}
	certs, err := f.store.DeviceCertificates().ListForDevice(ctx, deviceID)
	if err != nil {
		t.Fatalf("list certificates: %v", err)
	}
	if len(certs) != 1 {
		t.Errorf("expected 1 certificate after commit, got %d", len(certs))
	}
}

// --- certificates ----------------------------------------------------------

func TestCertificateLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	dev := f.enrollDevice(t, f.acme, "CERT-1", "cert-mac")

	issued, err := f.store.DeviceCertificates().Issue(ctx, &domain.DeviceCertificate{
		DeviceID:          dev.ID,
		Serial:            "112233445566778899001122",
		FingerprintSHA256: hashToken("cert-1"),
		PublicKeyDER:      []byte{0x30, 0x2a},
		CertificatePEM:    "PEM",
		NotBefore:         timeNow().Add(-time.Minute),
		NotAfter:          timeNow().Add(90 * 24 * time.Hour),
		IssuedVia:         "enrollment",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// A 20-byte X.509 serial exceeds int64, so it must survive as a decimal
	// string rather than being truncated through an integer type.
	if issued.Serial != "112233445566778899001122" {
		t.Errorf("serial did not round-trip: %q", issued.Serial)
	}
	if !issued.Usable(time.Now()) {
		t.Error("a freshly issued certificate should be usable")
	}
	if issued.NeedsRenewal(time.Now(), 30*24*time.Hour) {
		t.Error("a 90-day certificate should not need renewal on day one")
	}
	if !issued.NeedsRenewal(time.Now(), 100*24*time.Hour) {
		t.Error("renewal should be due when the window exceeds the remaining lifetime")
	}

	current, err := f.store.DeviceCertificates().Current(ctx, dev.ID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current.ID != issued.ID {
		t.Error("current certificate mismatch")
	}

	// Revocation must reach the set the mTLS callback consults.
	if err := f.store.DeviceCertificates().Revoke(ctx, issued.ID, "compromised", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revoked, err := f.store.DeviceCertificates().ListRevokedFingerprints(t.Context())
	if err != nil {
		t.Fatalf("list revoked: %v", err)
	}
	found := false
	for _, fp := range revoked {
		if string(fp) == string(hashToken("cert-1")) {
			found = true
		}
	}
	if !found {
		t.Error("a revoked certificate must appear in the revocation set")
	}

	if _, err := f.store.DeviceCertificates().Current(ctx, dev.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a revoked certificate must not be current, got: %v", err)
	}
}

// Decommissioning must revoke certificates too, or the device could reconnect
// with a certificate for a device that no longer exists.
func TestDecommissionRevokesCertificates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := f.asOrg(t, f.acme)

	dev := f.enrollDevice(t, f.acme, "DECOM-1", "decom-mac")
	if _, err := f.store.DeviceCertificates().Issue(ctx, &domain.DeviceCertificate{
		DeviceID:          dev.ID,
		Serial:            "5",
		FingerprintSHA256: hashToken("decom-cert"),
		PublicKeyDER:      []byte{0x30},
		CertificatePEM:    "PEM",
		NotBefore:         timeNow().Add(-time.Minute),
		NotAfter:          timeSoon(),
		IssuedVia:         "enrollment",
	}); err != nil {
		t.Fatalf("issue: %v", err)
	}

	if err := f.store.Devices().Decommission(ctx, dev.ID); err != nil {
		t.Fatalf("decommission: %v", err)
	}

	revoked, err := f.store.DeviceCertificates().ListRevokedFingerprints(t.Context())
	if err != nil {
		t.Fatalf("list revoked: %v", err)
	}
	found := false
	for _, fp := range revoked {
		if string(fp) == string(hashToken("decom-cert")) {
			found = true
		}
	}
	if !found {
		t.Error("decommissioning must revoke the device's certificates")
	}

	// The serial is freed for a genuinely new device.
	if _, err := f.store.Devices().Enroll(ctx, domain.EnrollDeviceInput{
		DeviceID:     mustUUIDv7(t),
		OrgID:        f.acme.ID,
		SerialNumber: "DECOM-1",
		Hostname:     "replacement",
	}); err != nil {
		t.Errorf("a decommissioned serial should be re-enrollable: %v", err)
	}
}
