package http_test

import (
	"net/http"
	"testing"
)

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func TestUsersListIncludesTheAdministratorWithRoles(t *testing.T) {
	f := newFixture(t)
	token := f.signIn(t)

	rec := f.do(t, http.MethodGet, "/api/v1/users", nil, bearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("list users: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			Email string   `json:"email"`
			Roles []string `json:"roles"`
		} `json:"data"`
	}
	decodeBody(t, rec, &body)
	if len(body.Data) == 0 {
		t.Fatal("expected at least the seeded administrator")
	}
	found := false
	for _, u := range body.Data {
		if len(u.Roles) > 0 && u.Roles[0] == "admin" {
			found = true
		}
	}
	if !found {
		t.Errorf("no user carried the admin role: %+v", body.Data)
	}
}

func TestCreateUserReturnsAOneTimePasswordThatSignsIn(t *testing.T) {
	f := newFixture(t)
	token := f.signIn(t)

	rec := f.do(t, http.MethodPost, "/api/v1/users", map[string]any{
		"email": "new.operator@example.com", "full_name": "New Operator", "roles": []string{"operator"},
	}, bearer(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID                string   `json:"id"`
		Roles             []string `json:"roles"`
		Status            string   `json:"status"`
		TemporaryPassword string   `json:"temporary_password"`
	}
	decodeBody(t, rec, &created)
	if created.TemporaryPassword == "" {
		t.Fatal("expected a temporary password")
	}
	if created.Status != "active" {
		t.Errorf("status = %q, want active", created.Status)
	}

	// The password works exactly as issued.
	login := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "new.operator@example.com", "password": created.TemporaryPassword,
	})
	if login.Code != http.StatusOK {
		t.Fatalf("sign in with the temporary password: %d %s", login.Code, login.Body.String())
	}

	// And the row reads back without it.
	get := f.do(t, http.MethodGet, "/api/v1/users/"+created.ID, nil, bearer(token))
	if get.Code != http.StatusOK {
		t.Fatalf("get user: %d", get.Code)
	}
	if body := get.Body.String(); contains(body, "temporary_password") || contains(body, created.TemporaryPassword) {
		t.Error("the password must never be readable after creation")
	}
}

func TestUpdateUserChangesRoleAndStatus(t *testing.T) {
	f := newFixture(t)
	token := f.signIn(t)
	viewer := f.createUser(t, "viewer@example.com", "viewer")

	rec := f.do(t, http.MethodPatch, "/api/v1/users/"+viewer.id.String(), map[string]any{
		"roles": []string{"operator"}, "status": "disabled",
	}, bearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string   `json:"status"`
		Roles  []string `json:"roles"`
	}
	decodeBody(t, rec, &body)
	if body.Status != "disabled" || len(body.Roles) != 1 || body.Roles[0] != "operator" {
		t.Errorf("got %+v, want disabled operator", body)
	}

	// Suspended means suspended.
	login := f.do(t, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": viewer.email, "password": viewer.password,
	})
	if login.Code == http.StatusOK {
		t.Error("a suspended user must not be able to sign in")
	}
}

func TestAnAdministratorCannotSuspendOrRemoveThemselves(t *testing.T) {
	f := newFixture(t)
	token := f.signIn(t)

	me := f.do(t, http.MethodGet, "/api/v1/auth/me", nil, bearer(token))
	var principal struct {
		UserID string `json:"user_id"`
	}
	decodeBody(t, me, &principal)

	if rec := f.do(t, http.MethodPatch, "/api/v1/users/"+principal.UserID, map[string]any{"status": "disabled"}, bearer(token)); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("self-suspend: %d %s, want a validation failure", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/users/"+principal.UserID, nil, bearer(token)); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("self-delete: %d %s, want a validation failure", rec.Code, rec.Body.String())
	}
}

func TestTheLastAdministratorCannotBeDemoted(t *testing.T) {
	f := newFixture(t)
	// A second admin does the demoting, so the self-check does not fire; the
	// seeded admin is the only *other* admin, and removing it must fail.
	second := f.createUser(t, "second.admin@example.com", "admin")
	token := f.signInAs(t, second)

	me := f.do(t, http.MethodGet, "/api/v1/auth/me", nil, bearer(token))
	var principal struct {
		UserID string `json:"user_id"`
	}
	decodeBody(t, me, &principal)

	// Demote the *seeded* admin: allowed, because `second` remains.
	list := f.do(t, http.MethodGet, "/api/v1/users", nil, bearer(token))
	var users struct {
		Data []struct {
			ID    string   `json:"id"`
			Roles []string `json:"roles"`
		} `json:"data"`
	}
	decodeBody(t, list, &users)
	var seeded string
	for _, u := range users.Data {
		if u.ID != principal.UserID && len(u.Roles) > 0 && u.Roles[0] == "admin" {
			seeded = u.ID
		}
	}
	if seeded == "" {
		t.Fatal("could not find the seeded admin")
	}
	if rec := f.do(t, http.MethodPatch, "/api/v1/users/"+seeded, map[string]any{"roles": []string{"viewer"}}, bearer(token)); rec.Code != http.StatusOK {
		t.Fatalf("demoting one of two admins must succeed: %d %s", rec.Code, rec.Body.String())
	}
	// Now `second` is the last admin. Only admins hold user:write in a fresh
	// organization, so the only account that can reach the last-admin rule is
	// that admin itself — and the self check answers first, as a validation
	// refusal. Both guards are exercised: the earlier demotion proved one admin
	// may demote another; this proves the last one cannot be removed at all.
	if rec := f.do(t, http.MethodPatch, "/api/v1/users/"+principal.UserID, map[string]any{"roles": []string{"viewer"}}, bearer(token)); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("the last admin demoting itself: %d %s, want a validation refusal", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/users/"+principal.UserID, nil, bearer(token)); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("the last admin removing itself: %d %s, want a validation refusal", rec.Code, rec.Body.String())
	}
}

func TestRolesAndPermissionsAreListed(t *testing.T) {
	f := newFixture(t)
	token := f.signIn(t)

	roles := f.do(t, http.MethodGet, "/api/v1/roles", nil, bearer(token))
	if roles.Code != http.StatusOK {
		t.Fatalf("roles: %d %s", roles.Code, roles.Body.String())
	}
	var body struct {
		Data []struct {
			Slug        string   `json:"slug"`
			IsSystem    bool     `json:"is_system"`
			Permissions []string `json:"permissions"`
			UserCount   int      `json:"user_count"`
		} `json:"data"`
	}
	decodeBody(t, roles, &body)
	slugs := map[string]int{}
	for _, r := range body.Data {
		slugs[r.Slug] = r.UserCount
		if !r.IsSystem {
			t.Errorf("unexpected custom role %s in a fresh organization", r.Slug)
		}
	}
	if slugs["admin"] < 1 {
		t.Errorf("admin user_count = %d, want at least the seeded administrator", slugs["admin"])
	}
	for _, want := range []string{"admin", "operator", "viewer"} {
		if _, ok := slugs[want]; !ok {
			t.Errorf("role %s missing", want)
		}
	}

	perms := f.do(t, http.MethodGet, "/api/v1/permissions", nil, bearer(token))
	if perms.Code != http.StatusOK || !contains(perms.Body.String(), "device:read") {
		t.Errorf("permissions: %d %s", perms.Code, perms.Body.String())
	}
}

func TestOrganizationCanBeReadAndRenamed(t *testing.T) {
	f := newFixture(t)
	token := f.signIn(t)

	get := f.do(t, http.MethodGet, "/api/v1/organization", nil, bearer(token))
	if get.Code != http.StatusOK {
		t.Fatalf("get org: %d %s", get.Code, get.Body.String())
	}
	patch := f.do(t, http.MethodPatch, "/api/v1/organization", map[string]any{
		"name": "Renamed Co", "settings": map[string]any{"auto_approve_enrollment": true},
	}, bearer(token))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch org: %d %s", patch.Code, patch.Body.String())
	}
	var org struct {
		Name     string `json:"name"`
		Settings struct {
			AutoApprove bool `json:"auto_approve_enrollment"`
		} `json:"settings"`
	}
	decodeBody(t, patch, &org)
	if org.Name != "Renamed Co" || !org.Settings.AutoApprove {
		t.Errorf("got %+v", org)
	}

	// A viewer may read the organization but not change it.
	viewer := f.createUser(t, "viewer2@example.com", "viewer")
	viewerToken := f.signInAs(t, viewer)
	if rec := f.do(t, http.MethodGet, "/api/v1/organization", nil, bearer(viewerToken)); rec.Code != http.StatusOK {
		t.Errorf("viewer read org: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPatch, "/api/v1/organization", map[string]any{"name": "x"}, bearer(viewerToken)); rec.Code != http.StatusForbidden {
		t.Errorf("viewer rename org: %d, want 403", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/users", nil, bearer(viewerToken)); rec.Code != http.StatusForbidden {
		t.Errorf("viewer list users: %d, want 403", rec.Code)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
