package http

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
	"github.com/visionify/edgepilot/apps/server/internal/service/enrollment"
)

// maxTokenLifetime caps how far ahead an enrollment token may expire.
const maxTokenLifetime = 30 * 24 * time.Hour

// --- enrollment tokens (operators) -----------------------------------------

type enrollmentTokenResponse struct {
	ID          string            `json:"id"`
	OrgID       string            `json:"org_id"`
	TokenPrefix string            `json:"token_prefix"`
	Description string            `json:"description,omitempty"`
	MaxUses     *int              `json:"max_uses,omitempty"`
	UseCount    int               `json:"use_count"`
	AutoApprove bool              `json:"auto_approve"`
	Labels      map[string]string `json:"labels,omitempty"`
	ExpiresAt   time.Time         `json:"expires_at"`
	RevokedAt   *time.Time        `json:"revoked_at,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Usable      bool              `json:"usable"`

	// Token is the plaintext, present only in the creation response. It is
	// never stored — only its SHA-256 is — so this is the one and only time it
	// can be read.
	Token string `json:"token,omitempty"`
}

type createEnrollmentTokenRequest struct {
	Description string            `json:"description"`
	MaxUses     *int              `json:"max_uses"`
	AutoApprove bool              `json:"auto_approve"`
	Labels      map[string]string `json:"labels"`
	// ExpiresAt is absolute, per the contract. Validated against the server's
	// clock, not the client's: a client whose clock is a week fast would
	// otherwise mint a token that outlives what its operator intended.
	ExpiresAt time.Time `json:"expires_at"`
}

// createEnrollmentToken mints a token for installing agents.
func (s *Server) createEnrollmentToken(w http.ResponseWriter, r *http.Request) error {
	principal, err := principalFrom(r.Context())
	if err != nil {
		return err
	}

	var req createEnrollmentTokenRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	now := s.now()
	var problems []domain.FieldError

	if strings.TrimSpace(req.Description) == "" {
		// Required by the contract, and worth requiring: a list of unlabelled
		// tokens is a list an operator cannot safely revoke from.
		problems = append(problems, domain.FieldError{
			Pointer: "/description", Message: "is required",
		})
	}
	switch {
	case req.ExpiresAt.IsZero():
		problems = append(problems, domain.FieldError{
			Pointer: "/expires_at", Message: "is required",
		})
	case !req.ExpiresAt.After(now):
		problems = append(problems, domain.FieldError{
			Pointer: "/expires_at", Message: "must be in the future",
		})
	case req.ExpiresAt.After(now.Add(maxTokenLifetime)):
		// A token that outlives the machines it installs is a standing
		// credential. Thirty days is already generous for a staged rollout.
		problems = append(problems, domain.FieldError{
			Pointer: "/expires_at", Message: "must be at most 30 days away",
		})
	}
	if len(problems) > 0 {
		return domain.Validation(problems...)
	}

	plaintext, hash, prefix, err := token.NewEnrollmentToken()
	if err != nil {
		return domain.Internal(err)
	}

	created, err := s.store.EnrollmentTokens().Create(r.Context(),
		domain.CreateEnrollmentTokenInput{
			OrgID:       principal.OrgID,
			Description: req.Description,
			MaxUses:     req.MaxUses,
			AutoApprove: req.AutoApprove,
			Labels:      req.Labels,
			ExpiresAt:   req.ExpiresAt,
			CreatedBy:   &principal.UserID,
		}, hash, prefix)
	if err != nil {
		return err
	}

	body := renderEnrollmentToken(created, now)
	body.Token = plaintext
	return respond(w, http.StatusCreated, body)
}

// listEnrollmentTokens returns the tokens for the caller's organization.
//
// The plaintext is absent from every entry: only the SHA-256 is stored, so it
// cannot be recovered. The prefix is enough to tell two tokens apart in a list.
func (s *Server) listEnrollmentTokens(w http.ResponseWriter, r *http.Request) error {
	page, err := pageRequest(r)
	if err != nil {
		return err
	}

	result, err := s.store.EnrollmentTokens().List(r.Context(), page)
	if err != nil {
		return err
	}

	now := s.now()
	items := make([]enrollmentTokenResponse, 0, len(result.Items))
	for _, t := range result.Items {
		items = append(items, renderEnrollmentToken(t, now))
	}
	return respond(w, http.StatusOK, listResponse[enrollmentTokenResponse]{
		Data: items, NextCursor: result.NextCursor, HasMore: result.HasMore,
	})
}

// revokeEnrollmentToken makes a token unusable immediately.
func (s *Server) revokeEnrollmentToken(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.store.EnrollmentTokens().Revoke(r.Context(), id); err != nil {
		return err
	}
	return noContent(w)
}

func renderEnrollmentToken(t *domain.EnrollmentToken, now time.Time) enrollmentTokenResponse {
	return enrollmentTokenResponse{
		ID:          t.ID.String(),
		OrgID:       t.OrgID.String(),
		TokenPrefix: t.TokenPrefix,
		Description: t.Description,
		MaxUses:     t.MaxUses,
		UseCount:    t.UseCount,
		AutoApprove: t.AutoApprove,
		Labels:      t.Labels,
		ExpiresAt:   t.ExpiresAt,
		RevokedAt:   t.RevokedAt,
		CreatedAt:   t.CreatedAt,
		Usable:      t.Usable(now),
	}
}

// --- agent enrollment ------------------------------------------------------

type agentEnrollRequest struct {
	DeviceID        string `json:"device_id"`
	CSR             string `json:"csr"`
	SerialNumber    string `json:"serial_number"`
	HardwareUUID    string `json:"hardware_uuid"`
	Hostname        string `json:"hostname"`
	OSVersion       string `json:"os_version"`
	OSBuild         string `json:"os_build"`
	ModelIdentifier string `json:"model_identifier"`
	Chip            string `json:"chip"`
	Arch            string `json:"arch"`
	CPUCores        *int   `json:"cpu_cores"`
	MemoryBytes     *int64 `json:"memory_bytes"`
	DiskTotalBytes  *int64 `json:"disk_total_bytes"`
	AgentVersion    string `json:"agent_version"`
	LocalIP         string `json:"local_ip"`
}

type agentEnrollResponse struct {
	DeviceID            string   `json:"device_id"`
	OrgID               string   `json:"org_id"`
	CertificatePEM      string   `json:"certificate_pem"`
	CABundlePEM         string   `json:"ca_bundle_pem"`
	NotAfter            string   `json:"not_after"`
	Approved            bool     `json:"approved"`
	DevicePlaneEndpoint string   `json:"device_plane_endpoint"`
	SigningPublicKeys   []string `json:"signing_public_keys,omitempty"`
}

// enrollAgent issues a device's first certificate.
//
// # Why this is the only user-plane endpoint an agent touches
//
// Everything else an agent does goes over the mTLS device plane, which requires
// a client certificate. This is where that certificate comes from, so it cannot
// itself require one.
//
// The credential is an enrollment token, which is deliberately weaker than a
// session: it is scoped to one organization, TTL-bounded, use-counted, and
// hashed at rest. What it can do is register a device into its own
// organization. What it cannot do is choose which organization, impersonate an
// existing device, or obtain anything that authorises a command — a pending
// device is dispatched nothing.
func (s *Server) enrollAgent(w http.ResponseWriter, r *http.Request) error {
	if s.enrollment == nil {
		return domain.Internal(errNoEnrollmentService)
	}

	// Rate limited by source before anything else. A leaked token is worth far
	// less if it cannot be redeemed at speed, and this endpoint is
	// unauthenticated by necessity.
	if allowed, retryAfter := s.agentLimits.Allow("enroll:" + ClientIP(r)); !allowed {
		return domain.RateLimited(retryAfter)
	}

	plaintext, err := bearerToken(r)
	if err != nil {
		// The same answer for a missing token as for a bad one: an agent learns
		// only that enrollment was refused.
		return domain.Unauthenticated("invalid_enrollment_token")
	}

	var req agentEnrollRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	deviceID, err := uuidFrom(req.DeviceID, "/device_id")
	if err != nil {
		return err
	}

	csrDER, err := base64.StdEncoding.DecodeString(req.CSR)
	if err != nil || len(csrDER) == 0 {
		return domain.Validation(domain.FieldError{
			Pointer: "/csr", Message: "must be base64-encoded DER",
		})
	}

	result, err := s.enrollment.Enroll(r.Context(), enrollment.Request{
		Token:           plaintext,
		DeviceID:        deviceID,
		CSRDer:          csrDER,
		SerialNumber:    strings.TrimSpace(req.SerialNumber),
		HardwareUUID:    strings.TrimSpace(req.HardwareUUID),
		Hostname:        strings.TrimSpace(req.Hostname),
		ModelIdentifier: req.ModelIdentifier,
		Chip:            req.Chip,
		Arch:            req.Arch,
		CPUCores:        req.CPUCores,
		MemoryBytes:     req.MemoryBytes,
		DiskTotalBytes:  req.DiskTotalBytes,
		OSVersion:       req.OSVersion,
		OSBuild:         req.OSBuild,
		AgentVersion:    req.AgentVersion,
		LocalIP:         req.LocalIP,
		PublicIP:        ClientIP(r),
	})
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(result.SigningPublicKeys))
	for _, key := range result.SigningPublicKeys {
		keys = append(keys, base64.StdEncoding.EncodeToString(key))
	}

	endpoint := result.DevicePlaneEndpoint
	if endpoint == "" {
		endpoint = s.devicePlaneEndpoint
	}

	return respond(w, http.StatusCreated, agentEnrollResponse{
		DeviceID:            result.DeviceID.String(),
		OrgID:               result.OrgID.String(),
		CertificatePEM:      string(result.CertificatePEM),
		CABundlePEM:         string(result.CABundlePEM),
		NotAfter:            result.NotAfter.UTC().Format(time.RFC3339),
		Approved:            result.Approved,
		DevicePlaneEndpoint: endpoint,
		SigningPublicKeys:   keys,
	})
}
