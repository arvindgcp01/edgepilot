package collect

import (
	"context"
	"time"
)

// MetricsSample is one reading of the machine's health.
//
// Gauges are instantaneous; the byte counters are cumulative since boot and
// the server differences them into rates. Temperature is a pointer because
// most of this fleet is Apple silicon, whose die temperature has no readable
// sensor without a kernel extension — and the honest value there is "unknown",
// never zero.
type MetricsSample struct {
	At time.Time

	CPUPercent       float32
	CPUUserPercent   float32
	CPUSystemPercent float32
	Load1            float32
	Load5            float32
	Load15           float32

	MemoryUsedBytes  int64
	MemoryTotalBytes int64
	// MemoryPressure is 0–100: how far the system is from having free memory,
	// which on macOS is a better health signal than used/total, since the
	// kernel deliberately keeps memory occupied by cache.
	MemoryPressure float32
	SwapUsedBytes  int64

	DiskUsedBytes  int64
	DiskTotalBytes int64

	NetRxBytes int64
	NetTxBytes int64

	TemperatureC  *float32
	UptimeSeconds int64
	ProcessCount  int32

	TopProcesses []ProcessSample
}

// ProcessSample is one entry of the top-processes list.
type ProcessSample struct {
	PID         int32
	Name        string
	User        string
	CPUPercent  float32
	MemoryBytes int64
}

// MetricsCollector reads the machine's current health.
type MetricsCollector interface {
	Sample(ctx context.Context) (*MetricsSample, error)
}

// NewMetrics returns the platform's collector: real readings on a Mac, a
// plausible synthetic series everywhere else, so the pipeline can be exercised
// end to end on a Linux CI box.
func NewMetrics() MetricsCollector { return newMetricsCollector() }

// topProcessLimit caps the process list. Five is what fits in a tooltip.
const topProcessLimit = 5
