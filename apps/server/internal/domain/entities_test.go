package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// These are pure invariants — no database involved. They exist so the rules
// they encode can be changed deliberately rather than by accident, and so the
// service layer can rely on them without re-checking.

func TestUserCanAuthenticate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	for _, tc := range []struct {
		name string
		user *User
		want bool
	}{
		{"active", &User{Status: UserActive}, true},
		{"invited has no password yet", &User{Status: UserInvited}, false},
		{"disabled", &User{Status: UserDisabled}, false},
		{"locked status", &User{Status: UserLocked}, false},
		{"active but lock still in force", &User{Status: UserActive, LockedUntil: &future}, false},
		{"active with an expired lock", &User{Status: UserActive, LockedUntil: &past}, true},
		{"soft deleted", &User{Status: UserActive, DeletedAt: &past}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.user.CanAuthenticate(now); got != tc.want {
				t.Errorf("CanAuthenticate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrincipalAuthorize(t *testing.T) {
	t.Parallel()

	p := &Principal{
		UserID: uuid.New(),
		Permissions: map[string]struct{}{
			PermDeviceRead:     {},
			PermCommandExecute: {},
		},
	}

	if !p.Can(PermDeviceRead) {
		t.Error("should hold device:read")
	}
	if p.Can(PermCommandCustomScript) {
		t.Error("should not hold command:custom_script")
	}
	if err := p.Authorize(PermDeviceRead); err != nil {
		t.Errorf("Authorize should succeed: %v", err)
	}

	err := p.Authorize(PermCommandCustomScript)
	if err == nil {
		t.Fatal("Authorize should fail for a permission not held")
	}
	if KindOf(err) != KindForbidden {
		t.Errorf("kind = %v, want forbidden", KindOf(err))
	}

	// A nil principal holds nothing. An unauthenticated request reaching an
	// authorization check must be refused, not crash.
	var nilPrincipal *Principal
	if nilPrincipal.Can(PermDeviceRead) {
		t.Error("a nil principal must hold no permission")
	}
	if err := nilPrincipal.Authorize(PermDeviceRead); err == nil {
		t.Error("a nil principal must fail authorization")
	}
}

func TestDeviceApprovedAndOnline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)
	stale := now.Add(-10 * time.Minute)
	deleted := now.Add(-time.Hour)

	// A device can be connected and reporting while still unapproved — in which
	// case it must receive no commands.
	pending := &Device{EnrollmentState: EnrollmentPending, LastSeenAt: &recent}
	if pending.Approved() {
		t.Error("a pending device must not report approved")
	}
	if !pending.Online(now, 90*time.Second) {
		t.Error("a pending device can still be online")
	}

	approved := &Device{EnrollmentState: EnrollmentApproved, LastSeenAt: &recent}
	if !approved.Approved() {
		t.Error("an approved device should report approved")
	}

	if (&Device{EnrollmentState: EnrollmentApproved, DeletedAt: &deleted}).Approved() {
		t.Error("a deleted device must not report approved")
	}

	if (&Device{LastSeenAt: &stale}).Online(now, 90*time.Second) {
		t.Error("a device last seen 10 minutes ago is not online")
	}
	if (&Device{}).Online(now, 90*time.Second) {
		t.Error("a device never seen is not online")
	}
	var nilDevice *Device
	if nilDevice.Approved() || nilDevice.Online(now, time.Minute) {
		t.Error("a nil device is neither approved nor online")
	}
}

func TestEnrollmentTokenUsable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	for _, tc := range []struct {
		name  string
		token *EnrollmentToken
		want  bool
	}{
		{"unlimited uses, unexpired", &EnrollmentToken{ExpiresAt: future}, true},
		{"expired", &EnrollmentToken{ExpiresAt: past}, false},
		{"expiring exactly now", &EnrollmentToken{ExpiresAt: now}, false},
		{"revoked", &EnrollmentToken{ExpiresAt: future, RevokedAt: &past}, false},
		{"uses remaining", &EnrollmentToken{ExpiresAt: future, MaxUses: ptrTo(3), UseCount: 2}, true},
		{"uses exhausted", &EnrollmentToken{ExpiresAt: future, MaxUses: ptrTo(3), UseCount: 3}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.token.Usable(now); got != tc.want {
				t.Errorf("Usable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCertificateUsableAndRenewal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	revoked := now.Add(-time.Hour)

	valid := &DeviceCertificate{
		NotBefore: now.Add(-24 * time.Hour),
		NotAfter:  now.Add(90 * 24 * time.Hour),
	}
	if !valid.Usable(now) {
		t.Error("a certificate inside its validity window should be usable")
	}
	if valid.NeedsRenewal(now, 30*24*time.Hour) {
		t.Error("90 days remaining should not trigger a 30-day renewal window")
	}
	if !valid.NeedsRenewal(now, 100*24*time.Hour) {
		t.Error("a window wider than the remaining lifetime should trigger renewal")
	}

	// Revocation must take precedence over validity: a revoked certificate is
	// unusable even while it is still within its dates.
	revokedCert := &DeviceCertificate{
		NotBefore: now.Add(-24 * time.Hour),
		NotAfter:  now.Add(24 * time.Hour),
		RevokedAt: &revoked,
	}
	if revokedCert.Usable(now) {
		t.Error("a revoked certificate must be unusable even inside its window")
	}
	if revokedCert.NeedsRenewal(now, time.Hour) {
		t.Error("a revoked certificate is not renewed, it is replaced")
	}

	expired := &DeviceCertificate{NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(-time.Hour)}
	if expired.Usable(now) {
		t.Error("an expired certificate must be unusable")
	}
	notYetValid := &DeviceCertificate{NotBefore: now.Add(time.Hour), NotAfter: now.Add(48 * time.Hour)}
	if notYetValid.Usable(now) {
		t.Error("a certificate that is not yet valid must be unusable")
	}
}

func TestSessionActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	if !(&Session{ExpiresAt: now.Add(time.Hour)}).Active(now) {
		t.Error("an unexpired, unrevoked session is active")
	}
	if (&Session{ExpiresAt: past}).Active(now) {
		t.Error("an expired session is not active")
	}
	if (&Session{ExpiresAt: now.Add(time.Hour), RevokedAt: &past}).Active(now) {
		t.Error("a revoked session is not active")
	}
	var nilSession *Session
	if nilSession.Active(now) {
		t.Error("a nil session is not active")
	}
}

func TestValidationInputs(t *testing.T) {
	t.Parallel()

	t.Run("a user must have at least one role", func(t *testing.T) {
		t.Parallel()
		err := CreateUserInput{Email: "a@b.com", FullName: "A"}.Validate()
		if !IsKind(err, KindValidation) {
			t.Errorf("expected validation error, got %v", err)
		}
	})

	t.Run("email shape", func(t *testing.T) {
		t.Parallel()
		for _, email := range []string{"", "no-at-sign", "@nolocal.com", "trailing@", "spaces @b.com", "a@nodot"} {
			err := CreateUserInput{Email: email, RoleSlugs: []string{RoleViewer}}.Validate()
			if !IsKind(err, KindValidation) {
				t.Errorf("%q should be rejected", email)
			}
		}
		for _, email := range []string{"a@b.co", "first.last+tag@sub.example.com"} {
			if err := (CreateUserInput{Email: email, RoleSlugs: []string{RoleViewer}}).Validate(); err != nil {
				t.Errorf("%q should be accepted: %v", email, err)
			}
		}
	})

	t.Run("update rejects system-managed statuses", func(t *testing.T) {
		t.Parallel()
		// invited and locked are set by the system; an operator setting them
		// directly would desynchronise the account from its actual state.
		for _, s := range []UserStatus{UserInvited, UserLocked} {
			if err := (UpdateUserInput{Status: &s}).Validate(); !IsKind(err, KindValidation) {
				t.Errorf("status %q should be rejected in an update", s)
			}
		}
		for _, s := range []UserStatus{UserActive, UserDisabled} {
			if err := (UpdateUserInput{Status: &s}).Validate(); err != nil {
				t.Errorf("status %q should be accepted: %v", s, err)
			}
		}
	})

	t.Run("device enrollment", func(t *testing.T) {
		t.Parallel()
		valid := EnrollDeviceInput{DeviceID: uuid.New(), SerialNumber: "C02X", Hostname: "mac"}
		if err := valid.Validate(); err != nil {
			t.Errorf("valid input rejected: %v", err)
		}

		missingID := valid
		missingID.DeviceID = uuid.Nil
		if err := missingID.Validate(); !IsKind(err, KindValidation) {
			t.Error("a missing device id should be rejected")
		}

		noSerial := valid
		noSerial.SerialNumber = ""
		if err := noSerial.Validate(); !IsKind(err, KindValidation) {
			t.Error("a missing serial number should be rejected")
		}

		badArch := valid
		badArch.Arch = "powerpc"
		if err := badArch.Validate(); !IsKind(err, KindValidation) {
			t.Error("an unsupported architecture should be rejected")
		}
	})
}

func TestStatusValidators(t *testing.T) {
	t.Parallel()

	if !OrgActive.Valid() || OrgStatus("nonsense").Valid() {
		t.Error("OrgStatus.Valid is wrong")
	}
	if !UserActive.Valid() || UserStatus("nonsense").Valid() {
		t.Error("UserStatus.Valid is wrong")
	}
	if !DeviceStatusOnline.Valid() || DeviceStatus("nonsense").Valid() {
		t.Error("DeviceStatus.Valid is wrong")
	}
	if !EnrollmentApproved.Valid() || EnrollmentState("nonsense").Valid() {
		t.Error("EnrollmentState.Valid is wrong")
	}
	// The sort whitelist is what keeps an ordering parameter from becoming SQL.
	if !DeviceSortLastSeenDesc.Valid() || DeviceSort("; DROP TABLE devices --").Valid() {
		t.Error("DeviceSort.Valid is wrong")
	}
}

func ptrTo[T any](v T) *T { return &v }
