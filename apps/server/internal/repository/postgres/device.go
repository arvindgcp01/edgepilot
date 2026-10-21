package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

type deviceRepo struct{ s *Store }

func (r *deviceRepo) Enroll(ctx context.Context, in domain.EnrollDeviceInput) (*domain.Device, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	labels, err := labelsToJSON(in.Labels)
	if err != nil {
		return nil, domain.Internal(err)
	}

	state := domain.EnrollmentPending
	if in.AutoApprove {
		state = domain.EnrollmentApproved
	}

	var out *domain.Device
	err = r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.EnrollDevice(ctx, sqlcgen.EnrollDeviceParams{
			ID:              toPgUUID(in.DeviceID),
			OrgID:           toPgUUID(in.OrgID),
			SerialNumber:    in.SerialNumber,
			HardwareUuid:    ptrString(in.HardwareUUID),
			Hostname:        in.Hostname,
			ModelIdentifier: ptrString(in.ModelIdentifier),
			Chip:            ptrString(in.Chip),
			Arch:            ptrString(in.Arch),
			CpuCores:        ptrInt16(in.CPUCores),
			MemoryBytes:     in.MemoryBytes,
			DiskTotalBytes:  in.DiskTotalBytes,
			OsVersion:       ptrString(in.OSVersion),
			OsBuild:         ptrString(in.OSBuild),
			AgentVersion:    ptrString(in.AgentVersion),
			LocalIp:         toNetip(in.LocalIP),
			PublicIp:        toNetip(in.PublicIP),
			Labels:          labels,
			EnrolledByToken: toPgUUIDPtr(in.EnrolledByToken),
			EnrollmentState: string(state),
		})
		if err != nil {
			return translate(err, "device")
		}
		out = deviceFromRow(row)
		return nil
	})
	return out, err
}

func (r *deviceRepo) SetScreenSharing(ctx context.Context, id uuid.UUID, enabled bool, sealed []byte) (*domain.Device, error) {
	var out *domain.Device
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.SetDeviceScreenSharing(ctx, sqlcgen.SetDeviceScreenSharingParams{
			ID:             toPgUUID(id),
			Enabled:        enabled,
			SealedPassword: sealed,
		})
		if err != nil {
			return translate(err, "device")
		}
		out = deviceFromRow(row)
		return nil
	})
	return out, err
}

func (r *deviceRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Device, error) {
	var out *domain.Device
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetDevice(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "device")
		}
		out = deviceFromRow(row)
		return nil
	})
	return out, err
}

func (r *deviceRepo) GetBySerial(ctx context.Context, serial string) (*domain.Device, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errNoTenantScope)
	}
	var out *domain.Device
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.GetDeviceBySerial(ctx, sqlcgen.GetDeviceBySerialParams{
			OrgID:        toPgUUID(orgID),
			SerialNumber: serial,
		})
		if err != nil {
			return translate(err, "device")
		}
		out = deviceFromRow(row)
		return nil
	})
	return out, err
}

func (r *deviceRepo) List(ctx context.Context, filter domain.DeviceFilter, page domain.PageRequest) (domain.Page[*domain.Device], error) {
	page = page.Normalize()
	var result domain.Page[*domain.Device]

	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return result, domain.Internal(errNoTenantScope)
	}

	sort := filter.Sort
	if sort == "" {
		sort = domain.DeviceSortLastSeenDesc
	}
	if !sort.Valid() {
		// The sort reaches SQL as a value compared inside CASE arms, never as
		// interpolated text — but rejecting an unknown one still beats silently
		// falling back, which would return a differently-ordered page than the
		// caller's cursor was built against.
		return result, domain.Invalid("/sort", "is not a supported ordering")
	}

	params := sqlcgen.ListDevicesParams{
		OrgID:    toPgUUID(orgID),
		Sort:     string(sort),
		PageSize: int32(page.Limit + 1),
	}

	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, s := range filter.Statuses {
			if !s.Valid() {
				return result, domain.Invalid("/status", "%q is not a device status", string(s))
			}
			statuses = append(statuses, string(s))
		}
		params.Statuses = statuses
	}
	if filter.EnrollmentState != nil {
		if !filter.EnrollmentState.Valid() {
			return result, domain.Invalid("/enrollment_state", "is not a valid enrollment state")
		}
		params.EnrollmentState = ptrString(string(*filter.EnrollmentState))
	}
	if filter.Search != "" {
		params.Search = ptrString(escapeLike(filter.Search))
	}
	if len(filter.Labels) > 0 {
		labels, err := labelsToJSON(filter.Labels)
		if err != nil {
			return result, domain.Internal(err)
		}
		params.Labels = labels
	}
	if !page.Cursor.IsZero() {
		params.CursorID = toPgUUID(page.Cursor.ID)
		params.CursorTime = toPgTime(page.Cursor.Time)
		// The hostname orderings compare text, not time, so the cursor carries
		// the hostname it was built from.
		params.CursorText = ptrString(page.Cursor.Text)
	}

	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ListDevices(ctx, params)
		if err != nil {
			return translate(err, "device")
		}
		devices := make([]*domain.Device, 0, len(rows))
		for _, row := range rows {
			d := deviceFromRow(row.Device)
			d.ActiveAlertCount = int(row.ActiveAlertCount)
			devices = append(devices, d)
		}
		result = domain.NewPage(devices, page.Limit, func(d *domain.Device) domain.Cursor {
			return cursorForDevice(d, sort)
		})
		return nil
	})
	return result, err
}

func (r *deviceRepo) Update(ctx context.Context, id uuid.UUID, in domain.UpdateDeviceInput) (*domain.Device, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	var labels []byte
	if in.Labels != nil {
		var err error
		if labels, err = labelsToJSON(in.Labels); err != nil {
			return nil, domain.Internal(err)
		}
	}

	var out *domain.Device
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.UpdateDevice(ctx, sqlcgen.UpdateDeviceParams{
			ID:                 toPgUUID(id),
			ClientName:         in.ClientName,
			Site:               in.Site,
			Notes:              in.Notes,
			Labels:             labels,
			AllowCustomScripts: in.AllowCustomScripts,
		})
		if err != nil {
			return translate(err, "device")
		}
		out = deviceFromRow(row)
		return nil
	})
	return out, err
}

func (r *deviceRepo) SetEnrollmentState(ctx context.Context, id uuid.UUID, state domain.EnrollmentState) (*domain.Device, error) {
	if !state.Valid() {
		return nil, domain.Invalid("/enrollment_state", "is not a valid enrollment state")
	}
	var out *domain.Device
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.SetDeviceEnrollmentState(ctx, sqlcgen.SetDeviceEnrollmentStateParams{
			ID:              toPgUUID(id),
			EnrollmentState: string(state),
		})
		if err != nil {
			return translate(err, "device")
		}
		out = deviceFromRow(row)
		return nil
	})
	return out, err
}

func (r *deviceRepo) SetStatus(ctx context.Context, id uuid.UUID, status domain.DeviceStatus) error {
	if !status.Valid() {
		return domain.Invalid("/status", "is not a device status")
	}
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		// The query is a no-op when the status already matches, so zero rows
		// here means "already in that state", not "missing". Treating it as
		// not-found would make an idempotent transition look like a failure.
		if _, err := q.SetDeviceStatus(ctx, sqlcgen.SetDeviceStatusParams{
			ID:     toPgUUID(id),
			Status: string(status),
		}); err != nil {
			return translate(err, "device")
		}
		return nil
	})
}

func (r *deviceRepo) TouchLastSeen(ctx context.Context, seen map[uuid.UUID]time.Time) error {
	if len(seen) == 0 {
		return nil
	}

	ids := make([]pgtype.UUID, 0, len(seen))
	times := make([]pgtype.Timestamptz, 0, len(seen))
	for id, at := range seen {
		ids = append(ids, toPgUUID(id))
		times = append(times, toPgTime(at))
	}

	// Runs unscoped: the flusher batches heartbeats from every organization a
	// replica happens to be holding streams for, and splitting the batch per
	// tenant would defeat the point of batching. The device ids come from
	// authenticated agent streams, not from user input.
	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.TouchDevicesLastSeen(ctx, sqlcgen.TouchDevicesLastSeenParams{
			DeviceIds: ids,
			SeenAts:   times,
		}); err != nil {
			return translate(err, "device")
		}
		return nil
	})
}

func (r *deviceRepo) Decommission(ctx context.Context, id uuid.UUID) error {
	return r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		// Revoking the certificates first means a device cannot reconnect in
		// the window between the two statements. Both are in one transaction,
		// so a failure leaves neither applied.
		if _, err := q.RevokeDeviceCertificates(ctx, sqlcgen.RevokeDeviceCertificatesParams{
			DeviceID: toPgUUID(id),
			Reason:   ptrString("device decommissioned"),
		}); err != nil {
			return translate(err, "device")
		}
		n, err := q.DecommissionDevice(ctx, toPgUUID(id))
		if err != nil {
			return translate(err, "device")
		}
		if n == 0 {
			return notFound("device", id.String())
		}
		return nil
	})
}

func (r *deviceRepo) Summary(ctx context.Context, offlineAfter time.Duration) (*domain.FleetSummary, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errNoTenantScope)
	}

	var out *domain.FleetSummary
	err := r.s.scoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.DeviceFleetSummary(ctx, sqlcgen.DeviceFleetSummaryParams{
			OrgID: toPgUUID(orgID),
			// Populated once the agent release catalogue exists; until then the
			// count is reported as zero rather than guessed.
			CurrentAgentVersion: nil,
		})
		if err != nil {
			return translate(err, "device")
		}
		out = &domain.FleetSummary{
			Total:            int(row.Total),
			Online:           int(row.Online),
			Offline:          int(row.Offline),
			Degraded:         int(row.Degraded),
			PendingApproval:  int(row.PendingApproval),
			UpdatesAvailable: int(row.UpdatesAvailable),
		}
		return nil
	})
	return out, err
}

// MarkStaleOffline transitions devices whose last_seen_at has aged out, and
// returns those that changed so the caller can raise alerts for exactly them.
// Unscoped: the sweeper runs across every organization.
func (r *deviceRepo) MarkStaleOffline(ctx context.Context, threshold time.Time) ([]domain.StaleDevice, error) {
	var out []domain.StaleDevice
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.MarkStaleDevicesOffline(ctx, toPgTime(threshold))
		if err != nil {
			return translate(err, "device")
		}
		out = make([]domain.StaleDevice, 0, len(rows))
		for _, row := range rows {
			out = append(out, domain.StaleDevice{
				DeviceID: fromPgUUID(row.ID),
				OrgID:    fromPgUUID(row.OrgID),
				Hostname: row.Hostname,
			})
		}
		return nil
	})
	return out, err
}

func deviceFromRow(row sqlcgen.Device) *domain.Device {
	return &domain.Device{
		ID:                   fromPgUUID(row.ID),
		OrgID:                fromPgUUID(row.OrgID),
		Hostname:             row.Hostname,
		SerialNumber:         row.SerialNumber,
		HardwareUUID:         derefString(row.HardwareUuid),
		ModelIdentifier:      derefString(row.ModelIdentifier),
		ModelName:            derefString(row.ModelName),
		Chip:                 derefString(row.Chip),
		Arch:                 derefString(row.Arch),
		CPUCores:             intFromInt16(row.CpuCores),
		CPUPerformanceCores:  intFromInt16(row.CpuPerformanceCores),
		CPUEfficiencyCores:   intFromInt16(row.CpuEfficiencyCores),
		MemoryBytes:          row.MemoryBytes,
		DiskTotalBytes:       row.DiskTotalBytes,
		OSName:               row.OsName,
		OSVersion:            derefString(row.OsVersion),
		OSBuild:              derefString(row.OsBuild),
		AgentVersion:         derefString(row.AgentVersion),
		LocalIP:              fromNetip(row.LocalIp),
		PublicIP:             fromNetip(row.PublicIp),
		PrimaryMAC:           macString(row.PrimaryMac),
		ClientName:           row.ClientName,
		Site:                 row.Site,
		Notes:                row.Notes,
		Labels:               labelsFromJSON(row.Labels),
		Status:               domain.DeviceStatus(row.Status),
		EnrollmentState:      domain.EnrollmentState(row.EnrollmentState),
		AllowCustomScripts:   row.AllowCustomScripts,
		ScreenSharingEnabled: row.ScreenSharingEnabled,
		VNCPasswordSealed:    row.VncPasswordSealed,
		LastCPUPercent:       row.LastCpuPercent,
		LastMemoryPercent:    row.LastMemoryPercent,
		LastDiskPercent:      row.LastDiskPercent,
		FirstEnrolledAt:      fromPgTimePtr(row.FirstEnrolledAt),
		LastSeenAt:           fromPgTimePtr(row.LastSeenAt),
		LastBootAt:           fromPgTimePtr(row.LastBootAt),
		ConnectedReplica:     derefString(row.ConnectedReplica),
		EnrolledByToken:      fromPgUUIDPtr(row.EnrolledByToken),
		CreatedAt:            fromPgTime(row.CreatedAt),
		UpdatedAt:            fromPgTime(row.UpdatedAt),
		DeletedAt:            fromPgTimePtr(row.DeletedAt),
	}
}

// cursorForDevice builds the cursor matching the active ordering. A cursor from
// one sort is meaningless under another, which is why changing `sort` mid-
// pagination is rejected rather than silently honoured.
func cursorForDevice(d *domain.Device, sort domain.DeviceSort) domain.Cursor {
	c := domain.Cursor{ID: d.ID}
	switch sort {
	case domain.DeviceSortLastSeenAsc, domain.DeviceSortLastSeenDesc:
		if d.LastSeenAt != nil {
			c.Time = *d.LastSeenAt
		}
	case domain.DeviceSortHostnameAsc, domain.DeviceSortHostnameDesc:
		c.Text = d.Hostname
	default:
		c.Time = d.CreatedAt
	}
	return c
}

// escapeLike neutralises the wildcards in a user-supplied search term, so
// searching for "%" matches a literal percent sign rather than every device.
func escapeLike(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			out = append(out, '\\', r)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
