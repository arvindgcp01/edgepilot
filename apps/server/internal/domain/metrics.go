package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MetricSample is one reading of a device's health, as the agent reported it.
//
// Pointers mark what the hardware may genuinely not provide. A Mac with no
// readable SMC sensor has no temperature, and writing zero there would tell an
// operator the machine is at freezing point rather than that nobody knows. The
// same goes for counters an older agent does not send.
type MetricSample struct {
	DeviceID uuid.UUID
	Time     time.Time

	CPUPercent       float32
	CPUUserPercent   *float32
	CPUSystemPercent *float32
	Load1            *float32
	Load5            *float32
	Load15           *float32

	MemoryUsedBytes  *int64
	MemoryTotalBytes *int64
	MemoryPressure   *float32
	SwapUsedBytes    *int64

	DiskUsedBytes  *int64
	DiskTotalBytes *int64
	// Monotonic since boot. Rates are derived by whoever reads two samples.
	DiskReadBytes  *int64
	DiskWriteBytes *int64
	NetRxBytes     *int64
	NetTxBytes     *int64

	TemperatureC  *float32
	UptimeSeconds *int64
	ProcessCount  *int32
}

// MemoryPercent derives utilisation from the byte counts, when both are known.
func (m *MetricSample) MemoryPercent() *float32 {
	return percentOf(m.MemoryUsedBytes, m.MemoryTotalBytes)
}

// DiskPercent derives utilisation from the byte counts, when both are known.
func (m *MetricSample) DiskPercent() *float32 {
	return percentOf(m.DiskUsedBytes, m.DiskTotalBytes)
}

func percentOf(used, total *int64) *float32 {
	if used == nil || total == nil || *total <= 0 {
		return nil
	}
	v := float32(float64(*used) / float64(*total) * 100)
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return &v
}

// LastMetrics is the denormalised utilisation the fleet table shows, kept on
// the devices row so listing the fleet never touches the metrics partitions.
type LastMetrics struct {
	CPUPercent    float32
	MemoryPercent *float32
	DiskPercent   *float32
	// BootedAt is derived from uptime. Reported here because a metrics sample
	// is the one message that carries uptime on every tick.
	BootedAt *time.Time
	At       time.Time
}

// MetricBucket is one point of a time series: the average of every sample
// that fell inside the bucket, with counters carried as their maximum so a
// rate can be derived from consecutive buckets.
type MetricBucket struct {
	Bucket time.Time

	CPUPercent       *float32
	Load1            *float32
	Load5            *float32
	Load15           *float32
	MemoryUsedBytes  *int64
	MemoryTotalBytes *int64
	MemoryPressure   *float32
	DiskUsedBytes    *int64
	DiskTotalBytes   *int64
	DiskReadBytes    *int64
	DiskWriteBytes   *int64
	NetRxBytes       *int64
	NetTxBytes       *int64
	TemperatureC     *float32
	UptimeSeconds    *int64
	ProcessCount     *int32
}

// MetricSeriesQuery selects a window of one device's history.
type MetricSeriesQuery struct {
	DeviceID uuid.UUID
	From     time.Time
	To       time.Time
	// Bucket is the width every point is averaged over. Must be positive.
	Bucket time.Duration
}

// MetricsRepository stores and reads device health.
type MetricsRepository interface {
	// InsertSamples writes a batch of raw samples in one round trip. Samples
	// for a device that no longer exists are dropped, not failed: an agent
	// whose device was decommissioned mid-flush should not poison the batch
	// for every other device in it.
	InsertSamples(ctx context.Context, samples []MetricSample) error

	// UpdateLast writes the denormalised utilisation onto the devices rows.
	UpdateLast(ctx context.Context, last map[uuid.UUID]LastMetrics) error

	// Latest returns the most recent sample, or NotFound when the device has
	// never reported one.
	Latest(ctx context.Context, deviceID uuid.UUID) (*MetricSample, error)

	// Series returns bucketed history, oldest first. Buckets with no samples
	// are absent, not zero — the caller decides how to render a gap.
	Series(ctx context.Context, q MetricSeriesQuery) ([]MetricBucket, error)

	// EnsurePartitions creates the monthly partitions that hold samples for
	// the given months, if they do not already exist. Idempotent.
	EnsurePartitions(ctx context.Context, months []time.Time) error
}
