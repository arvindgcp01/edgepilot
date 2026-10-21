package client

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/visionify/edgepilot/apps/agent/internal/collect"
	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// metricsTimeout bounds one reading end to end. The collector has its own,
// shorter bound; this one exists so a hung collector cannot pin the goroutine
// past the next tick.
const metricsTimeout = 20 * time.Second

// collectMetrics takes one reading and offers it to the stream.
//
// Off the producer goroutine: a reading takes about a second (top samples
// twice), and the producer is also the heartbeat, which must not be late
// because the CPU figure was slow to arrive. The reading is perishable in the
// same way a heartbeat is — if the queue is full the next one is thirty
// seconds away — so it goes through offer, never the spool.
func (c *Client) collectMetrics(ctx context.Context, outbound chan<- *v1.AgentMessage) {
	if c.metrics == nil {
		return
	}
	if !c.collecting.CompareAndSwap(false, true) {
		c.log.Debug("skipping a metrics reading; the previous one is still running")
		return
	}
	go func() {
		defer c.collecting.Store(false)
		readCtx, cancel := context.WithTimeout(ctx, metricsTimeout)
		defer cancel()

		sample, err := c.metrics.Sample(readCtx)
		if err != nil {
			// Warn, not error: one failed reading is a gap in a chart. A Mac
			// whose readings fail every time shows up as a device with no
			// metrics on the dashboard, which is the right place to notice.
			c.log.Warn("reading metrics failed; this sample is skipped", slog.Any("error", err))
			return
		}
		c.offer(ctx, outbound, c.metricsMessage(sample))
	}()
}

// metricsMessage converts a reading to the wire.
func (c *Client) metricsMessage(s *collect.MetricsSample) *v1.AgentMessage {
	m := &v1.MetricsSample{
		CollectedAt:      timestamppb.New(s.At),
		CpuPercent:       s.CPUPercent,
		CpuUserPercent:   s.CPUUserPercent,
		CpuSystemPercent: s.CPUSystemPercent,
		Load_1:           s.Load1,
		Load_5:           s.Load5,
		Load_15:          s.Load15,
		MemoryUsedBytes:  nonNegative(s.MemoryUsedBytes),
		MemoryTotalBytes: nonNegative(s.MemoryTotalBytes),
		MemoryPressure:   s.MemoryPressure,
		SwapUsedBytes:    nonNegative(s.SwapUsedBytes),
		DiskUsedBytes:    nonNegative(s.DiskUsedBytes),
		DiskTotalBytes:   nonNegative(s.DiskTotalBytes),
		NetRxBytes:       nonNegative(s.NetRxBytes),
		NetTxBytes:       nonNegative(s.NetTxBytes),
		UptimeSeconds:    nonNegative(s.UptimeSeconds),
		ProcessCount:     uint32(max(s.ProcessCount, 0)),
	}
	if s.TemperatureC != nil {
		t := *s.TemperatureC
		m.TemperatureC = &t
	}
	for _, p := range s.TopProcesses {
		m.TopProcesses = append(m.TopProcesses, &v1.ProcessSample{
			Pid:         p.PID,
			Name:        p.Name,
			User:        p.User,
			CpuPercent:  p.CPUPercent,
			MemoryBytes: nonNegative(p.MemoryBytes),
		})
	}
	return &v1.AgentMessage{
		SentAt:  timestamppb.New(c.now()),
		Payload: &v1.AgentMessage_Metrics{Metrics: m},
	}
}

func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
