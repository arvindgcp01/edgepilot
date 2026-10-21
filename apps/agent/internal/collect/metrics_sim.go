//go:build !darwin || simulator

package collect

import (
	"context"
	"math"
	"runtime"
	"time"
)

// simulatedMetrics produces a plausible, slowly varying series so the whole
// pipeline — agent, server, dashboard — can be run on a Linux box or in CI
// without a Mac. The shape is deliberately obvious in a chart: a slow sine
// on CPU with a little noise, memory creeping, disk flat. Nobody should
// mistake it for a real machine, and nothing in it is random enough to make
// a test flaky.
type simulatedMetrics struct {
	started time.Time
	rx, tx  int64
}

func newMetricsCollector() MetricsCollector {
	return &simulatedMetrics{started: time.Now()}
}

func (c *simulatedMetrics) Sample(_ context.Context) (*MetricsSample, error) {
	now := time.Now().UTC()
	elapsed := now.Sub(c.started).Seconds()

	cpu := float32(25 + 20*math.Sin(elapsed/90) + 5*math.Sin(elapsed/7))
	total := simulatedMemoryBytes()
	used := int64(float64(total) * (0.45 + 0.1*math.Sin(elapsed/300)))

	c.rx += int64(120_000 + 80_000*math.Abs(math.Sin(elapsed/30)))
	c.tx += int64(40_000 + 20_000*math.Abs(math.Cos(elapsed/45)))

	temperature := float32(48 + 6*math.Sin(elapsed/120))

	return &MetricsSample{
		At:               now,
		CPUPercent:       cpu,
		CPUUserPercent:   cpu * 0.7,
		CPUSystemPercent: cpu * 0.3,
		Load1:            cpu / 100 * float32(runtime.NumCPU()),
		Load5:            cpu / 100 * float32(runtime.NumCPU()) * 0.9,
		Load15:           cpu / 100 * float32(runtime.NumCPU()) * 0.8,
		MemoryUsedBytes:  used,
		MemoryTotalBytes: total,
		MemoryPressure:   float32(float64(used) / float64(total) * 100),
		SwapUsedBytes:    0,
		DiskUsedBytes:    250 << 30,
		DiskTotalBytes:   500 << 30,
		NetRxBytes:       c.rx,
		NetTxBytes:       c.tx,
		TemperatureC:     &temperature,
		UptimeSeconds:    int64(now.Sub(processStart()).Seconds()),
		ProcessCount:     412,
		TopProcesses: []ProcessSample{
			{PID: 501, Name: "edgepilot-agent", User: "root", CPUPercent: 0.4, MemoryBytes: 24 << 20},
		},
	}, nil
}
