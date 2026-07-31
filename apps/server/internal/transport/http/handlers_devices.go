package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// deviceResponse is the wire shape of a device.
//
// Written out rather than serialising the domain type. A domain struct that is
// also a wire contract means every field added for internal use is published by
// accident, and the two change for entirely different reasons.
type deviceResponse struct {
	ID     string `json:"id"`
	OrgID  string `json:"org_id"`
	Status string `json:"status"`

	Hostname     string `json:"hostname"`
	SerialNumber string `json:"serial_number"`
	HardwareUUID string `json:"hardware_uuid,omitempty"`

	ModelIdentifier string `json:"model_identifier,omitempty"`
	ModelName       string `json:"model_name,omitempty"`
	Chip            string `json:"chip,omitempty"`
	Arch            string `json:"arch,omitempty"`
	CPUCores        *int   `json:"cpu_cores,omitempty"`
	MemoryBytes     *int64 `json:"memory_bytes,omitempty"`
	DiskTotalBytes  *int64 `json:"disk_total_bytes,omitempty"`

	OSName    string `json:"os_name,omitempty"`
	OSVersion string `json:"os_version,omitempty"`
	OSBuild   string `json:"os_build,omitempty"`

	AgentVersion string `json:"agent_version,omitempty"`
	LocalIP      string `json:"local_ip,omitempty"`
	PublicIP     string `json:"public_ip,omitempty"`

	Site               string            `json:"site,omitempty"`
	Notes              string            `json:"notes,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	AllowCustomScripts bool              `json:"allow_custom_scripts"`

	EnrollmentState string `json:"enrollment_state"`
	Approved        bool   `json:"approved"`

	FirstEnrolledAt *time.Time `json:"first_enrolled_at,omitempty"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
	LastBootAt      *time.Time `json:"last_boot_at,omitempty"`

	ActiveAlertCount int `json:"active_alert_count"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// listResponse is the pagination envelope from openapi.yaml.
//
// `data` rather than `items`, and next_cursor is always present rather than
// omitted when empty: the contract marks both required, and a client written
// against a schema that promises a key should not have to handle its absence.
type listResponse[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

// listDevices returns a page of the fleet.
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) error {
	page, err := pageRequest(r)
	if err != nil {
		return err
	}
	filter, err := deviceFilter(r)
	if err != nil {
		return err
	}

	result, err := s.store.Devices().List(r.Context(), filter, page)
	if err != nil {
		return err
	}

	items := make([]deviceResponse, 0, len(result.Items))
	for _, device := range result.Items {
		items = append(items, renderDevice(device))
	}
	return respond(w, http.StatusOK, listResponse[deviceResponse]{
		Data: items, NextCursor: result.NextCursor, HasMore: result.HasMore,
	})
}

// deviceFilter reads the filtering parameters.
func deviceFilter(r *http.Request) (domain.DeviceFilter, error) {
	filter := domain.DeviceFilter{
		Search: strings.TrimSpace(r.URL.Query().Get("q")),
	}

	for _, raw := range queryList(r, "status") {
		status := domain.DeviceStatus(raw)
		if !status.Valid() {
			return filter, domain.Validation(domain.FieldError{
				Pointer: "/status", Message: "is not a device status: " + raw,
			})
		}
		filter.Statuses = append(filter.Statuses, status)
	}

	if raw := r.URL.Query().Get("enrollment_state"); raw != "" {
		state := domain.EnrollmentState(raw)
		if !state.Valid() {
			return filter, domain.Validation(domain.FieldError{
				Pointer: "/enrollment_state", Message: "is not an enrollment state: " + raw,
			})
		}
		filter.EnrollmentState = &state
	}

	// Labels arrive as label=key:value, repeated. ANDed: a device must carry
	// every pair to match, which is what makes "site:hq and ring:canary" mean
	// what an operator expects.
	for _, pair := range queryList(r, "label") {
		key, value, found := strings.Cut(pair, ":")
		if !found || strings.TrimSpace(key) == "" {
			return filter, domain.Validation(domain.FieldError{
				Pointer: "/label", Message: "must be key:value, got " + pair,
			})
		}
		if filter.Labels == nil {
			filter.Labels = map[string]string{}
		}
		filter.Labels[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	// The sort is a closed set, checked here rather than passed through. A
	// free-text column name reaching the query builder is how an ordering
	// parameter becomes SQL injection.
	switch sort := r.URL.Query().Get("sort"); domain.DeviceSort(sort) {
	case "":
		filter.Sort = domain.DeviceSortLastSeenDesc
	case domain.DeviceSortLastSeenDesc, domain.DeviceSortLastSeenAsc,
		domain.DeviceSortHostnameAsc, domain.DeviceSortHostnameDesc,
		domain.DeviceSortCreatedDesc, domain.DeviceSortCreatedAsc:
		filter.Sort = domain.DeviceSort(sort)
	default:
		return filter, domain.Validation(domain.FieldError{
			Pointer: "/sort", Message: "is not a supported ordering: " + sort,
		})
	}

	return filter, nil
}

// getDevice returns one device.
func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}

	device, err := s.store.Devices().Get(r.Context(), id)
	if err != nil {
		// A device in another organization is invisible: the query is scoped by
		// RLS, so it returns not-found rather than forbidden. That is
		// deliberate — a 403 would confirm the id exists.
		return err
	}
	return respond(w, http.StatusOK, renderDevice(device))
}

type updateDeviceRequest struct {
	Site               *string           `json:"site"`
	Notes              *string           `json:"notes"`
	Labels             map[string]string `json:"labels"`
	AllowCustomScripts *bool             `json:"allow_custom_scripts"`
}

// updateDevice changes the operator-owned fields.
//
// Hardware and OS facts are not settable: they come from inventory, and an
// operator editing the chip name would make the record disagree with the
// machine.
func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}

	var req updateDeviceRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	device, err := s.store.Devices().Update(r.Context(), id, domain.UpdateDeviceInput{
		Site:               req.Site,
		Notes:              req.Notes,
		Labels:             req.Labels,
		AllowCustomScripts: req.AllowCustomScripts,
	})
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, renderDevice(device))
}

// approveDevice admits a pending device to the fleet.
func (s *Server) approveDevice(w http.ResponseWriter, r *http.Request) error {
	return s.setApproval(w, r, true)
}

// rejectDevice refuses a pending device.
func (s *Server) rejectDevice(w http.ResponseWriter, r *http.Request) error {
	return s.setApproval(w, r, false)
}

func (s *Server) setApproval(w http.ResponseWriter, r *http.Request, approve bool) error {
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if s.enrollment == nil {
		return domain.Internal(errNoEnrollmentService)
	}

	device, err := s.enrollment.Approve(r.Context(), id, approve)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, renderDevice(device))
}

// decommissionDevice revokes the device's certificates and soft-deletes it.
//
// DELETE, but not a hard delete: the audit trail, the command history, and the
// metrics for this device stay. What goes away is its ability to connect, and
// its serial number is freed so a replacement Mac can be enrolled.
func (s *Server) decommissionDevice(w http.ResponseWriter, r *http.Request) error {
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}

	if err := s.store.Devices().Decommission(r.Context(), id); err != nil {
		return err
	}
	return noContent(w)
}

type summaryResponse struct {
	Total            int `json:"total"`
	Online           int `json:"online"`
	Offline          int `json:"offline"`
	Degraded         int `json:"degraded"`
	PendingApproval  int `json:"pending_approval"`
	UpdatesAvailable int `json:"updates_available"`
}

// deviceSummary returns the counters the dashboard's cards show.
func (s *Server) deviceSummary(w http.ResponseWriter, r *http.Request) error {
	summary, err := s.store.Devices().Summary(r.Context(), s.offlineAfter)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, summaryResponse{
		Total:            summary.Total,
		Online:           summary.Online,
		Offline:          summary.Offline,
		Degraded:         summary.Degraded,
		PendingApproval:  summary.PendingApproval,
		UpdatesAvailable: summary.UpdatesAvailable,
	})
}

func renderDevice(d *domain.Device) deviceResponse {
	out := deviceResponse{
		ID:                 d.ID.String(),
		OrgID:              d.OrgID.String(),
		Status:             string(d.Status),
		Hostname:           d.Hostname,
		SerialNumber:       d.SerialNumber,
		HardwareUUID:       d.HardwareUUID,
		ModelIdentifier:    d.ModelIdentifier,
		ModelName:          d.ModelName,
		Chip:               d.Chip,
		Arch:               d.Arch,
		MemoryBytes:        d.MemoryBytes,
		DiskTotalBytes:     d.DiskTotalBytes,
		OSName:             d.OSName,
		OSVersion:          d.OSVersion,
		OSBuild:            d.OSBuild,
		AgentVersion:       d.AgentVersion,
		LocalIP:            d.LocalIP,
		PublicIP:           d.PublicIP,
		Site:               d.Site,
		Notes:              d.Notes,
		Labels:             d.Labels,
		AllowCustomScripts: d.AllowCustomScripts,
		EnrollmentState:    string(d.EnrollmentState),
		Approved:           d.Approved(),
		FirstEnrolledAt:    d.FirstEnrolledAt,
		LastSeenAt:         d.LastSeenAt,
		LastBootAt:         d.LastBootAt,
		ActiveAlertCount:   d.ActiveAlertCount,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}
	if d.CPUCores != nil {
		out.CPUCores = d.CPUCores
	}
	return out
}

var errNoEnrollmentService = &configurationError{
	what: "the enrollment service is not wired into the API",
}

type configurationError struct{ what string }

func (e *configurationError) Error() string { return e.what }
