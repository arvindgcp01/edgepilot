//go:build darwin && !simulator

package collect

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// darwinMetrics reads health from a real Mac.
//
// Everything shells out, for the reason darwinCollector does: a static,
// cgo-free binary. The cost is a few dozen milliseconds of process spawning
// every thirty seconds, which on the fleet's slowest Mac mini is a rounding
// error in the CPU figure it is measuring.
//
// One reading is a best-effort composite. CPU, memory and disk are required —
// a sample without them is not worth sending — and everything else degrades
// to "unknown" on its own, so one missing tool does not blind the whole
// sample.
type darwinMetrics struct {
	// powermetrics is only useful on Intel, and is slow to fail on Apple
	// silicon. Once it has reported no sensor, it is not asked again.
	noTemperatureSensor bool
}

func newMetricsCollector() MetricsCollector { return &darwinMetrics{} }

// sampleTimeout bounds the whole reading. top alone takes a little over a
// second because it samples twice; anything past ten seconds is a Mac in
// trouble, and the sample would be stale by the time it arrived.
const sampleTimeout = 10 * time.Second

func (c *darwinMetrics) Sample(ctx context.Context) (*MetricsSample, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()

	sample := &MetricsSample{At: time.Now().UTC()}
	var errs []error

	// --- required -------------------------------------------------------

	// top -l 2 samples twice one second apart; the second reading is the
	// real one. -n 0 lists no processes, which is most of top's cost.
	if out, err := run(ctx, "top", "-l", "2", "-n", "0", "-s", "1"); err != nil {
		errs = append(errs, err)
	} else if user, system, idle, err := ParseTopCPU(out); err != nil {
		errs = append(errs, err)
	} else {
		sample.CPUUserPercent = user
		sample.CPUSystemPercent = system
		sample.CPUPercent = clamp(100-idle, 0, 100)
	}

	if out, err := run(ctx, "sysctl", "-n", "hw.memsize"); err != nil {
		errs = append(errs, err)
	} else if total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err != nil {
		errs = append(errs, fmt.Errorf("collect: hw.memsize: %w", err))
	} else {
		sample.MemoryTotalBytes = total
	}

	if out, err := run(ctx, "vm_stat"); err != nil {
		errs = append(errs, err)
	} else if stat, err := ParseVMStat(out); err != nil {
		errs = append(errs, err)
	} else {
		sample.MemoryUsedBytes = stat.Used()
		if sample.MemoryTotalBytes > 0 && sample.MemoryUsedBytes > sample.MemoryTotalBytes {
			sample.MemoryUsedBytes = sample.MemoryTotalBytes
		}
	}

	sample.DiskUsedBytes, sample.DiskTotalBytes = diskUsage("/")

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// --- best effort ----------------------------------------------------

	if out, err := run(ctx, "memory_pressure"); err == nil {
		if pressure, err := ParseMemoryPressure(out); err == nil {
			sample.MemoryPressure = clamp(pressure, 0, 100)
		}
	}
	if sample.MemoryPressure == 0 && sample.MemoryTotalBytes > 0 {
		// memory_pressure is absent on some builds; used/total is a coarser
		// but honest substitute.
		sample.MemoryPressure = clamp(float32(float64(sample.MemoryUsedBytes)/float64(sample.MemoryTotalBytes)*100), 0, 100)
	}

	if out, err := run(ctx, "sysctl", "-n", "vm.swapusage"); err == nil {
		if used, err := ParseSwapUsage(out); err == nil {
			sample.SwapUsedBytes = used
		}
	}

	if out, err := run(ctx, "sysctl", "-n", "vm.loadavg"); err == nil {
		if l1, l5, l15, err := ParseLoadAvg(out); err == nil {
			sample.Load1, sample.Load5, sample.Load15 = l1, l5, l15
		}
	}

	if out, err := run(ctx, "netstat", "-ibn"); err == nil {
		if rx, tx, err := ParseNetstatIB(out); err == nil {
			sample.NetRxBytes, sample.NetTxBytes = rx, tx
		}
	}

	if out, err := run(ctx, "sysctl", "-n", "kern.boottime"); err == nil {
		if booted, err := ParseBootTime(out); err == nil {
			if up := sample.At.Sub(booted); up > 0 {
				sample.UptimeSeconds = int64(up.Seconds())
			}
		}
	}

	if out, err := run(ctx, "ps", "-Ao", "pid="); err == nil {
		sample.ProcessCount = countLines(out)
	}
	if out, err := run(ctx, "ps", "-Aro", "pid=,pcpu=,rss=,user=,comm="); err == nil {
		sample.TopProcesses = ParseProcessList(out, topProcessLimit)
	}

	if !c.noTemperatureSensor {
		sample.TemperatureC = c.temperature(ctx)
		if sample.TemperatureC == nil {
			c.noTemperatureSensor = true
		}
	}

	return sample, nil
}

// temperature asks powermetrics for the CPU die temperature.
//
// Root-only, which the agent is. Apple silicon reports no SMC sampler and the
// command fails; that is the expected outcome on most of the fleet and is
// treated as "no sensor", not as an error worth logging every thirty seconds.
func (c *darwinMetrics) temperature(ctx context.Context) *float32 {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powermetrics", "--samplers", "smc", "-i", "1", "-n", "1")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	value, ok := ParseTemperature(out)
	if !ok {
		return nil
	}
	return &value
}

func clamp(v, lo, hi float32) float32 {
	if v != v { // NaN
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
