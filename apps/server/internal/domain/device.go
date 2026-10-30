package domain

import (
	"time"

	"github.com/google/uuid"
)

// DeviceStatus is what the fleet table shows in the Status column.
type DeviceStatus string

const (
	DeviceStatusPending        DeviceStatus = "pending"
	DeviceStatusOnline         DeviceStatus = "online"
	DeviceStatusOffline        DeviceStatus = "offline"
	DeviceStatusDegraded       DeviceStatus = "degraded"
	DeviceStatusDecommissioned DeviceStatus = "decommissioned"
)

func (s DeviceStatus) Valid() bool {
	switch s {
	case DeviceStatusPending, DeviceStatusOnline, DeviceStatusOffline,
		DeviceStatusDegraded, DeviceStatusDecommissioned:
		return true
	}
	return false
}

// EnrollmentState is separate from DeviceStatus on purpose: a device can be
// online (connected, reporting inventory) while still unapproved, and in that
// state it must receive no commands.
type EnrollmentState string

const (
	EnrollmentPending  EnrollmentState = "pending"
	EnrollmentApproved EnrollmentState = "approved"
	EnrollmentRejected EnrollmentState = "rejected"
	EnrollmentRevoked  EnrollmentState = "revoked"
)

func (s EnrollmentState) Valid() bool {
	switch s {
	case EnrollmentPending, EnrollmentApproved, EnrollmentRejected, EnrollmentRevoked:
		return true
	}
	return false
}

// Device is one Mac in the fleet.
type Device struct {
	ID    uuid.UUID
	OrgID uuid.UUID

	Hostname     string
	SerialNumber string
	HardwareUUID string

	// Hardware, refreshed from inventory. Pointers where the value is genuinely
	// unknown until the first report — zero would be a lie.
	ModelIdentifier     string
	ModelName           string
	Chip                string
	Arch                string
	CPUCores            *int
	CPUPerformanceCores *int
	CPUEfficiencyCores  *int
	MemoryBytes         *int64
	DiskTotalBytes      *int64

	OSName       string
	OSVersion    string
	OSBuild      string
	AgentVersion string

	LocalIP    string
	PublicIP   string
	PrimaryMAC string

	// ClientName is who the device belongs to, as an operator would say it.
	// Unlike Hostname it is set here and never by the agent — see the note on
	// ReportedFacts for why the two cannot be the same field.
	ClientName string

	Site   string
	Notes  string
	Labels map[string]string

	Status          DeviceStatus
	EnrollmentState EnrollmentState

	// ScreenSharingEnabled turns on macOS Screen Sharing for remote control.
	// The plaintext VNC password lives nowhere; VNCPasswordSealed holds it
	// sealed, and both the agent (to configure Screen Sharing) and the relay
	// (to authenticate) unseal the same value.
	ScreenSharingEnabled bool
	VNCPasswordSealed    []byte

	// AllowCustomScripts is per-device policy pushed to the agent, which
	// enforces it locally. A device with it false refuses a custom script even
	// when the payload carries a valid server signature.
	AllowCustomScripts bool

	// Denormalised utilisation, kept current by the metrics flusher so the fleet
	// table never joins a partitioned 28-million-row-per-day table.
	LastCPUPercent    *float32
	LastMemoryPercent *float32
	LastDiskPercent   *float32

	FirstEnrolledAt  *time.Time
	LastSeenAt       *time.Time
	LastBootAt       *time.Time
	ConnectedReplica string

	EnrolledByToken *uuid.UUID

	// ActiveAlertCount is joined in by listings so the fleet table can badge a
	// row without a second round trip per device.
	ActiveAlertCount int

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// StaleDevice identifies a device the offline sweeper just transitioned, so the
// caller can raise an alert for exactly those that changed rather than
// re-querying and racing the next sweep.
type StaleDevice struct {
	DeviceID uuid.UUID
	OrgID    uuid.UUID
	Hostname string
}

// Approved reports whether the device may be dispatched commands.
func (d *Device) Approved() bool {
	return d != nil && d.EnrollmentState == EnrollmentApproved && d.DeletedAt == nil
}

// Online reports whether the device was seen within the offline threshold.
//
// This is the durable answer, derived from last_seen_at. The live answer comes
// from the Redis presence key; they disagree for at most one flush interval,
// and the durable one is what a report or an audit should use.
func (d *Device) Online(now time.Time, offlineAfter time.Duration) bool {
	if d == nil || d.LastSeenAt == nil {
		return false
	}
	return now.Sub(*d.LastSeenAt) < offlineAfter
}

// EnrollDeviceInput registers a device on first boot.
type EnrollDeviceInput struct {
	// DeviceID is the UUIDv7 the agent generated and will keep for its lifetime.
	DeviceID uuid.UUID
	OrgID    uuid.UUID

	SerialNumber string
	HardwareUUID string
	Hostname     string

	ModelIdentifier string
	Chip            string
	Arch            string
	CPUCores        *int
	MemoryBytes     *int64
	DiskTotalBytes  *int64
	OSVersion       string
	OSBuild         string
	AgentVersion    string
	LocalIP         string
	PublicIP        string

	// Labels are stamped from the enrollment token, so a site or rollout ring
	// can be assigned at install time rather than by hand afterwards.
	Labels          map[string]string
	EnrolledByToken *uuid.UUID
	AutoApprove     bool
}

func (in EnrollDeviceInput) Validate() error {
	var fields []FieldError
	if in.DeviceID == uuid.Nil {
		fields = append(fields, FieldError{Pointer: "/device_id", Message: "is required"})
	}
	if l := len(in.SerialNumber); l < 1 || l > 64 {
		fields = append(fields, FieldError{Pointer: "/serial_number", Message: "must be 1 to 64 characters"})
	}
	if len(in.Hostname) > 253 {
		fields = append(fields, FieldError{Pointer: "/hostname", Message: "must be at most 253 characters"})
	}
	if in.Arch != "" && in.Arch != "arm64" && in.Arch != "x86_64" {
		fields = append(fields, FieldError{Pointer: "/arch", Message: "must be arm64 or x86_64"})
	}
	if err := validateLabels(in.Labels); err != nil {
		fields = append(fields, FieldError{Pointer: "/labels", Message: err.Error()})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// ReportedFacts are the values an agent sends in Hello, on every connect.
//
// Separate from UpdateDeviceInput because these are *observations* rather than
// operator intent: the device is the authority on them, and nothing in the
// dashboard should be able to set them. Keeping the two apart is what stops a
// PATCH from claiming a hostname the Mac does not have.
type ReportedFacts struct {
	Hostname     string
	OSName       string
	OSVersion    string
	OSBuild      string
	AgentVersion string
	LocalIP      string
}

// DiffersFrom reports whether any fact has changed since the row was written.
//
// Checked in Go before touching PostgreSQL: this runs on every reconnect for
// every device in the fleet, and an UPDATE that changes nothing is still a write,
// a WAL record, and a row version for autovacuum to clean up.
//
// Empty values are ignored rather than treated as a change. An agent that cannot
// determine its OS build sends "", and letting that erase a value already known
// would make the fleet listing worse on every reconnect.
func (f ReportedFacts) DiffersFrom(d *Device) bool {
	switch {
	case f.Hostname != "" && f.Hostname != d.Hostname:
		return true
	case f.OSName != "" && f.OSName != d.OSName:
		return true
	case f.OSVersion != "" && f.OSVersion != d.OSVersion:
		return true
	case f.OSBuild != "" && f.OSBuild != d.OSBuild:
		return true
	case f.AgentVersion != "" && f.AgentVersion != d.AgentVersion:
		return true
	case f.LocalIP != "" && f.LocalIP != d.LocalIP:
		return true
	}
	return false
}

// UpdateDeviceInput carries the operator-owned fields only. Hardware and OS
// facts come from inventory and are not settable here — an operator editing the
// chip name would make the record disagree with the machine.
//
// Every field is a pointer so that "absent" and "cleared" stay distinguishable:
// a PATCH that omits client_name must leave it alone, while one that sends ""
// must erase it. A plain string could only express one of those.
type UpdateDeviceInput struct {
	ClientName         *string
	Site               *string
	Notes              *string
	Labels             map[string]string
	AllowCustomScripts *bool
}

func (in UpdateDeviceInput) Validate() error {
	var fields []FieldError
	if in.ClientName != nil && len(*in.ClientName) > 200 {
		fields = append(fields, FieldError{Pointer: "/client_name", Message: "must be at most 200 characters"})
	}
	if in.Site != nil && len(*in.Site) > 200 {
		fields = append(fields, FieldError{Pointer: "/site", Message: "must be at most 200 characters"})
	}
	if in.Notes != nil && len(*in.Notes) > 4000 {
		fields = append(fields, FieldError{Pointer: "/notes", Message: "must be at most 4000 characters"})
	}
	if err := validateLabels(in.Labels); err != nil {
		fields = append(fields, FieldError{Pointer: "/labels", Message: err.Error()})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// DeviceFilter scopes a device listing.
type DeviceFilter struct {
	Statuses        []DeviceStatus
	EnrollmentState *EnrollmentState
	// Search is matched against hostname, client name, serial number, and site.
	Search string
	// Labels are ANDed: a device must carry every pair to match.
	Labels map[string]string
	Sort   DeviceSort
}

// DeviceSort names an ordering. It is a closed set rather than a free-text
// column name, so a sort parameter can never become SQL injection.
type DeviceSort string

const (
	DeviceSortLastSeenDesc DeviceSort = "-last_seen_at"
	DeviceSortLastSeenAsc  DeviceSort = "last_seen_at"
	DeviceSortHostnameAsc  DeviceSort = "hostname"
	DeviceSortHostnameDesc DeviceSort = "-hostname"
	DeviceSortCreatedDesc  DeviceSort = "-created_at"
	DeviceSortCreatedAsc   DeviceSort = "created_at"
)

func (s DeviceSort) Valid() bool {
	switch s {
	case DeviceSortLastSeenDesc, DeviceSortLastSeenAsc,
		DeviceSortHostnameAsc, DeviceSortHostnameDesc,
		DeviceSortCreatedDesc, DeviceSortCreatedAsc:
		return true
	}
	return false
}

// FleetSummary backs the dashboard cards.
type FleetSummary struct {
	Total            int
	Online           int
	Offline          int
	Degraded         int
	PendingApproval  int
	UpdatesAvailable int
}

// DeviceCertificate is one issued mTLS client certificate.
//
// EdgePilot holds only the public half: the agent generates its key on the Mac
// and sends a CSR, so there is no device private key on the server to steal.
type DeviceCertificate struct {
	ID                uuid.UUID
	DeviceID          uuid.UUID
	Serial            string
	FingerprintSHA256 []byte
	PublicKeyDER      []byte
	CertificatePEM    string
	NotBefore         time.Time
	NotAfter          time.Time
	IssuedVia         string // enrollment | renewal
	RevokedAt         *time.Time
	RevokedReason     string
	RevokedBy         *uuid.UUID
	CreatedAt         time.Time
}

// Usable reports whether the certificate should be accepted right now.
func (c *DeviceCertificate) Usable(now time.Time) bool {
	return c != nil && c.RevokedAt == nil &&
		now.After(c.NotBefore) && now.Before(c.NotAfter)
}

// NeedsRenewal reports whether the agent should be told to renew.
func (c *DeviceCertificate) NeedsRenewal(now time.Time, before time.Duration) bool {
	return c != nil && c.RevokedAt == nil && now.Add(before).After(c.NotAfter)
}

// EnrollmentToken authorises a device to enrol.
//
// Only the SHA-256 of the token is stored; the plaintext is shown once at
// creation and is not recoverable.
type EnrollmentToken struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	TokenPrefix string
	Description string
	MaxUses     *int
	UseCount    int
	AutoApprove bool
	Labels      map[string]string
	ExpiresAt   time.Time
	RevokedAt   *time.Time
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
}

// Usable reports whether the token may still enrol a device.
func (t *EnrollmentToken) Usable(now time.Time) bool {
	switch {
	case t == nil, t.RevokedAt != nil:
		return false
	case !t.ExpiresAt.After(now):
		return false
	case t.MaxUses != nil && t.UseCount >= *t.MaxUses:
		return false
	}
	return true
}

// CreateEnrollmentTokenInput creates a token.
type CreateEnrollmentTokenInput struct {
	OrgID       uuid.UUID
	Description string
	MaxUses     *int
	AutoApprove bool
	Labels      map[string]string
	ExpiresAt   time.Time
	CreatedBy   *uuid.UUID
}

func (in CreateEnrollmentTokenInput) Validate(now time.Time) error {
	var fields []FieldError
	if len(in.Description) > 200 {
		fields = append(fields, FieldError{Pointer: "/description", Message: "must be at most 200 characters"})
	}
	if !in.ExpiresAt.After(now) {
		fields = append(fields, FieldError{Pointer: "/expires_at", Message: "must be in the future"})
	}
	if in.MaxUses != nil && *in.MaxUses < 1 {
		fields = append(fields, FieldError{Pointer: "/max_uses", Message: "must be at least 1"})
	}
	if err := validateLabels(in.Labels); err != nil {
		fields = append(fields, FieldError{Pointer: "/labels", Message: err.Error()})
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// validateLabels bounds label cardinality and size.
//
// Labels drive deployment target selectors and alert-rule scoping, and they are
// GIN-indexed. Unbounded keys would let one operator degrade query planning for
// their whole organization.
func validateLabels(labels map[string]string) error {
	const (
		maxLabels   = 32
		maxKeyLen   = 64
		maxValueLen = 200
	)
	if len(labels) > maxLabels {
		return errLabelCount
	}
	for k, v := range labels {
		if k == "" || len(k) > maxKeyLen {
			return errLabelKey
		}
		if len(v) > maxValueLen {
			return errLabelValue
		}
	}
	return nil
}

type labelError string

func (e labelError) Error() string { return string(e) }

const (
	errLabelCount labelError = "at most 32 labels are allowed"
	errLabelKey   labelError = "label keys must be 1 to 64 characters"
	errLabelValue labelError = "label values must be at most 200 characters"
)
