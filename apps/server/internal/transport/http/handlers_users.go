package http

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// userResponse is the wire shape of an operator account.
//
// Like deviceResponse, written out rather than serialised from the domain
// type — and this one matters more, because domain.User's neighbours carry
// password hashes and MFA secrets, and a struct that is also a wire contract
// publishes whatever gets added to it.
type userResponse struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	FullName   string     `json:"full_name"`
	Status     string     `json:"status"`
	Roles      []string   `json:"roles"`
	MFAEnabled bool       `json:"mfa_enabled"`
	LastLogin  *time.Time `json:"last_login_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func renderUser(u *domain.User, roles []string) userResponse {
	if roles == nil {
		roles = []string{}
	}
	return userResponse{
		ID:         u.ID.String(),
		Email:      u.Email,
		FullName:   u.FullName,
		Status:     string(u.Status),
		Roles:      roles,
		MFAEnabled: u.MFAEnabled,
		LastLogin:  u.LastLoginAt,
		CreatedAt:  u.CreatedAt,
		UpdatedAt:  u.UpdatedAt,
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) error {
	page, err := pageRequest(r)
	if err != nil {
		return err
	}
	result, err := s.store.Users().List(r.Context(), page)
	if err != nil {
		return err
	}
	roles, err := s.store.Users().RolesForOrg(r.Context())
	if err != nil {
		return err
	}
	items := make([]userResponse, 0, len(result.Items))
	for _, u := range result.Items {
		items = append(items, renderUser(u, roles[u.ID]))
	}
	return respond(w, http.StatusOK, listResponse[userResponse]{
		Data: items, NextCursor: result.NextCursor, HasMore: result.HasMore,
	})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	user, err := s.store.Users().Get(r.Context(), id)
	if err != nil {
		return err
	}
	roles, _, err := s.store.Users().ResolvePermissions(r.Context(), id)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, renderUser(user, roles))
}

type createUserRequest struct {
	Email    string   `json:"email"`
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
}

// createUserResponse carries the one-time password. Present only here, and
// only once — the same shape as a freshly minted enrollment token.
type createUserResponse struct {
	userResponse
	TemporaryPassword string `json:"temporary_password"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	var req createUserRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	creator := principal.UserID
	user, temporary, err := s.auth.CreateUser(r.Context(), domain.CreateUserInput{
		OrgID:     principal.OrgID,
		Email:     strings.TrimSpace(strings.ToLower(req.Email)),
		FullName:  strings.TrimSpace(req.FullName),
		RoleSlugs: req.Roles,
		CreatedBy: &creator,
	})
	if err != nil {
		return err
	}
	return respond(w, http.StatusCreated, createUserResponse{
		userResponse:      renderUser(user, req.Roles),
		TemporaryPassword: temporary,
	})
}

type updateUserRequest struct {
	FullName *string  `json:"full_name"`
	Status   *string  `json:"status"`
	Roles    []string `json:"roles"`
}

// updateUser changes a name, a status, or the role set.
//
// Two things it refuses. An administrator cannot suspend or demote their own
// account — the request that removes your access should not be one you can
// send by accident — and the organization can never be left without an active
// administrator, because the only way back from that is a database console.
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var req updateUserRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	in := domain.UpdateUserInput{FullName: req.FullName, RoleSlugs: req.Roles}
	if req.Status != nil {
		status := domain.UserStatus(*req.Status)
		if status != domain.UserActive && status != domain.UserDisabled {
			return domain.Validation(domain.FieldError{Pointer: "/status", Message: "must be active or disabled"})
		}
		in.Status = &status
	}
	if req.Roles != nil && len(req.Roles) == 0 {
		return domain.Validation(domain.FieldError{Pointer: "/roles", Message: "at least one role is required"})
	}

	self := id == principal.UserID
	demoting := (in.Status != nil && *in.Status != domain.UserActive) ||
		(req.Roles != nil && !slices.Contains(req.Roles, domain.RoleAdmin))
	if self && demoting {
		return domain.Validation(domain.FieldError{
			Pointer: "/", Message: "you cannot suspend or demote your own account; ask another administrator",
		})
	}
	if demoting {
		if err := s.ensureAnotherAdmin(r, id); err != nil {
			return err
		}
	}

	user, err := s.store.Users().Update(r.Context(), id, in)
	if err != nil {
		return err
	}
	roles, _, err := s.store.Users().ResolvePermissions(r.Context(), id)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, renderUser(user, roles))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if id == principal.UserID {
		return domain.Validation(domain.FieldError{
			Pointer: "/", Message: "you cannot remove your own account; ask another administrator",
		})
	}
	if err := s.ensureAnotherAdmin(r, id); err != nil {
		return err
	}
	if err := s.store.Users().SoftDelete(r.Context(), id); err != nil {
		return err
	}
	return noContent(w)
}

// ensureAnotherAdmin refuses a change that would leave no active admin other
// than the user being changed.
func (s *Server) ensureAnotherAdmin(r *http.Request, changing uuid.UUID) error {
	roles, err := s.store.Users().RolesForOrg(r.Context())
	if err != nil {
		return err
	}
	page, err := s.store.Users().List(r.Context(), domain.PageRequest{Limit: 500})
	if err != nil {
		return err
	}
	for _, u := range page.Items {
		if u.ID != changing && u.Status == domain.UserActive && slices.Contains(roles[u.ID], domain.RoleAdmin) {
			return nil
		}
	}
	return domain.Conflict("last_admin", "this is the organization's only active administrator")
}

// --- roles and permissions ------------------------------------------------

type roleResponse struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	IsSystem    bool     `json:"is_system"`
	Permissions []string `json:"permissions"`
	UserCount   int      `json:"user_count"`
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) error {
	roles, err := s.store.Roles().List(r.Context())
	if err != nil {
		return err
	}
	holders, err := s.store.Users().RolesForOrg(r.Context())
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, slugs := range holders {
		for _, slug := range slugs {
			counts[slug]++
		}
	}
	out := make([]roleResponse, 0, len(roles))
	for _, role := range roles {
		perms := role.Permissions
		if perms == nil {
			perms = []string{}
		}
		out = append(out, roleResponse{
			ID: role.ID.String(), Slug: role.Slug, Name: role.Name, Description: role.Description,
			IsSystem: role.IsSystem, Permissions: perms, UserCount: counts[role.Slug],
		})
	}
	return respond(w, http.StatusOK, listResponse[roleResponse]{Data: out})
}

func (s *Server) listPermissions(w http.ResponseWriter, r *http.Request) error {
	slugs, err := s.store.Roles().ListPermissions(r.Context())
	if err != nil {
		return err
	}
	if slugs == nil {
		slugs = []string{}
	}
	return respond(w, http.StatusOK, listResponse[string]{Data: slugs})
}

// --- organization ----------------------------------------------------------

type organizationResponse struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Slug      string             `json:"slug"`
	Status    string             `json:"status"`
	Settings  domain.OrgSettings `json:"settings"`
	Devices   int                `json:"device_count"`
	CreatedAt time.Time          `json:"created_at"`
}

func (s *Server) getOrganization(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	org, err := s.store.Organizations().Get(r.Context(), principal.OrgID)
	if err != nil {
		return err
	}
	devices, err := s.store.Organizations().CountDevices(r.Context(), principal.OrgID)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, organizationResponse{
		ID: org.ID.String(), Name: org.Name, Slug: org.Slug, Status: string(org.Status),
		Settings: org.Settings, Devices: devices, CreatedAt: org.CreatedAt,
	})
}

type updateOrganizationRequest struct {
	Name     *string             `json:"name"`
	Settings *domain.OrgSettings `json:"settings"`
}

func (s *Server) updateOrganization(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}
	var req updateOrganizationRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" || len(trimmed) > 200 {
			return domain.Validation(domain.FieldError{Pointer: "/name", Message: "must be 1–200 characters"})
		}
		req.Name = &trimmed
	}
	org, err := s.store.Organizations().Update(r.Context(), principal.OrgID, domain.UpdateOrganizationInput{
		Name: req.Name, Settings: req.Settings,
	})
	if err != nil {
		return err
	}
	devices, err := s.store.Organizations().CountDevices(r.Context(), principal.OrgID)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, organizationResponse{
		ID: org.ID.String(), Name: org.Name, Slug: org.Slug, Status: string(org.Status),
		Settings: org.Settings, Devices: devices, CreatedAt: org.CreatedAt,
	})
}
