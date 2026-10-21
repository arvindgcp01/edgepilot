package grpc

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// MetricsRecorder receives device health samples from the stream handler.
//
// Declared here and satisfied by worker.MetricsFlusher, like PresenceRecorder:
// the dependency runs from the transport to the worker, and stating the
// interface where it is used keeps it that way. Record must not block — it is
// on the hot path of every agent's stream.
type MetricsRecorder interface {
	Record(sample domain.MetricSample)
}

// EventPublisher fans a device event out to dashboards.
//
// Satisfied in main by an adapter over the HTTP hub. Optional: a deployment
// with no dashboard connected has nothing to tell, and a nil publisher is
// cheaper than an interface that swallows.
type EventPublisher interface {
	PublishDeviceEvent(orgID, deviceID uuid.UUID, kind string, data any)
}

// Event kinds published on the fleet and device topics.
const (
	EventDeviceConnected    = "device.connected"
	EventDeviceDisconnected = "device.disconnected"
	EventDeviceMetrics      = "device.metrics"
)

// metricsEvent is the payload of EventDeviceMetrics: enough for a fleet table
// to update a row in place without a refetch, and no more.
type metricsEvent struct {
	CPUPercent    float32  `json:"cpu_percent"`
	MemoryPercent *float32 `json:"memory_percent"`
	DiskPercent   *float32 `json:"disk_percent"`
	UptimeSeconds *int64   `json:"uptime_seconds"`
	At            string   `json:"at"`
}

// maxSampleSkew bounds how far a sample's own timestamp may sit from the
// server's clock. An agent whose clock is wrong should not be able to write
// readings into next year, or into a partition that has been dropped.
const maxSampleSkew = 10 * time.Minute

// handleMetrics records one health sample.
//
// The sample is validated, not trusted: percentages are clamped, byte counts
// that would be nonsense are dropped to "unknown", and the timestamp is
// clamped to the server's clock. Then it goes to the flusher, which owns the
// database round trip, and to the dashboards, which want it now.
func (s *AgentService) handleMetrics(ctx context.Context, ep *Stream, m *v1.MetricsSample) error {
	if m == nil {
		return nil
	}
	now := s.now().UTC()

	at := now
	if ts := m.GetCollectedAt(); ts != nil && ts.IsValid() {
		at = ts.AsTime().UTC()
		if at.After(now.Add(maxSampleSkew)) || at.Before(now.Add(-maxSampleSkew)) {
			s.log.DebugContext(ctx, "clamping a metrics sample with a skewed timestamp",
				slog.String("device_id", ep.DeviceID.String()),
				slog.Time("reported", at), slog.Time("server", now))
			at = now
		}
	}

	sample := domain.MetricSample{
		DeviceID:         ep.DeviceID,
		Time:             at,
		CPUPercent:       clampPercent(m.GetCpuPercent()),
		CPUUserPercent:   optPercent(m.GetCpuUserPercent()),
		CPUSystemPercent: optPercent(m.GetCpuSystemPercent()),
		Load1:            optFloat(m.GetLoad_1()),
		Load5:            optFloat(m.GetLoad_5()),
		Load15:           optFloat(m.GetLoad_15()),
		MemoryUsedBytes:  optBytes(m.GetMemoryUsedBytes()),
		MemoryTotalBytes: optBytes(m.GetMemoryTotalBytes()),
		MemoryPressure:   optPercent(m.GetMemoryPressure()),
		SwapUsedBytes:    optBytes(m.GetSwapUsedBytes()),
		DiskUsedBytes:    optBytes(m.GetDiskUsedBytes()),
		DiskTotalBytes:   optBytes(m.GetDiskTotalBytes()),
		DiskReadBytes:    optBytes(m.GetDiskReadBytes()),
		DiskWriteBytes:   optBytes(m.GetDiskWriteBytes()),
		NetRxBytes:       optBytes(m.GetNetRxBytes()),
		NetTxBytes:       optBytes(m.GetNetTxBytes()),
		UptimeSeconds:    optBytes(m.GetUptimeSeconds()),
	}
	// Temperature is proto3 optional for exactly this reason: absent is a
	// fact about the hardware, and zero would be a lie about it.
	if m.TemperatureC != nil && !math.IsNaN(float64(m.GetTemperatureC())) {
		t := m.GetTemperatureC()
		sample.TemperatureC = &t
	}
	if n := m.GetProcessCount(); n > 0 && n < math.MaxInt32 {
		count := int32(n)
		sample.ProcessCount = &count
	}
	// Byte totals below the used figure are a reporting bug, and a percentage
	// over 100 on the fleet table is how that bug would surface. Drop the pair.
	if sample.MemoryUsedBytes != nil && sample.MemoryTotalBytes != nil && *sample.MemoryUsedBytes > *sample.MemoryTotalBytes {
		sample.MemoryUsedBytes, sample.MemoryTotalBytes = nil, nil
	}
	if sample.DiskUsedBytes != nil && sample.DiskTotalBytes != nil && *sample.DiskUsedBytes > *sample.DiskTotalBytes {
		sample.DiskUsedBytes, sample.DiskTotalBytes = nil, nil
	}

	if s.metrics != nil {
		s.metrics.Record(sample)
	}
	if s.events != nil {
		s.events.PublishDeviceEvent(ep.OrgID, ep.DeviceID, EventDeviceMetrics, metricsEvent{
			CPUPercent:    sample.CPUPercent,
			MemoryPercent: sample.MemoryPercent(),
			DiskPercent:   sample.DiskPercent(),
			UptimeSeconds: sample.UptimeSeconds,
			At:            at.Format(time.RFC3339),
		})
	}
	return nil
}

func clampPercent(v float32) float32 {
	switch {
	case math.IsNaN(float64(v)) || v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}

// optPercent treats zero as "not reported". The one percentage where zero is
// a real reading — CPU — goes through clampPercent instead.
func optPercent(v float32) *float32 {
	if v <= 0 || math.IsNaN(float64(v)) {
		return nil
	}
	c := clampPercent(v)
	return &c
}

func optFloat(v float32) *float32 {
	if v < 0 || math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return nil
	}
	return &v
}

// optBytes treats zero as "not reported": no Mac has zero bytes of memory,
// and a counter that reads zero has not been sampled.
func optBytes(v uint64) *int64 {
	if v == 0 || v > math.MaxInt64 {
		return nil
	}
	n := int64(v)
	return &n
}
