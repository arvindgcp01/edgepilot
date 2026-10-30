//go:build linux && !simulator

package collect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// linuxMetrics reads the machine's health from procfs and sysfs.
//
// Files, not commands, wherever the kernel offers one: they are the same on
// every distribution, cost nothing to read, and are present on a minimal image
// with no procps installed. `ps` is used only for the top-process list, and the
// sample survives without it.
type linuxMetrics struct {
	root string
}

func newMetricsCollector() MetricsCollector { return &linuxMetrics{root: "/"} }

// cpuSampleInterval is the gap between the two /proc/stat readings. One second
// matches what top on a Mac measures over, so the two platforms' CPU figures
// mean the same thing on the dashboard.
const cpuSampleInterval = time.Second

func (c *linuxMetrics) Sample(ctx context.Context) (*MetricsSample, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	sample := &MetricsSample{At: time.Now().UTC()}
	var errs []error

	// --- required -------------------------------------------------------

	before, err := ParseProcStatCPU(c.read("proc/stat"))
	if err == nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cpuSampleInterval):
		}
		var after CPUTimes
		if after, err = ParseProcStatCPU(c.read("proc/stat")); err == nil {
			user, system, busy, ok := CPUPercents(before, after)
			if ok {
				sample.CPUUserPercent, sample.CPUSystemPercent, sample.CPUPercent = user, system, busy
			}
		}
	}
	if err != nil {
		errs = append(errs, err)
	}

	if mem, err := ParseMemInfo(c.read("proc/meminfo")); err != nil {
		errs = append(errs, err)
	} else {
		sample.MemoryTotalBytes = mem.Total
		sample.MemoryUsedBytes = mem.Used()
		sample.SwapUsedBytes = mem.SwapTotal - mem.SwapFree
		sample.MemoryPressure = clamp(float32(float64(mem.Used())/float64(mem.Total)*100), 0, 100)
	}

	sample.DiskUsedBytes, sample.DiskTotalBytes = diskUsage(c.path("/"))

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// --- best effort ----------------------------------------------------

	if l1, l5, l15, err := ParseProcLoadAvg(c.read("proc/loadavg")); err == nil {
		sample.Load1, sample.Load5, sample.Load15 = l1, l5, l15
	}
	if rx, tx, err := ParseProcNetDev(c.read("proc/net/dev")); err == nil {
		sample.NetRxBytes, sample.NetTxBytes = rx, tx
	}
	if fields := strings.Fields(string(c.read("proc/uptime"))); len(fields) > 0 {
		if up, err := strconv.ParseFloat(fields[0], 64); err == nil {
			sample.UptimeSeconds = int64(up)
		}
	}
	if entries, err := os.ReadDir(c.path("proc")); err == nil {
		var n int32
		for _, e := range entries {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				n++
			}
		}
		sample.ProcessCount = n
	}
	// procps ps sorts for us; the output columns match what ParseProcessList
	// reads on a Mac.
	if out, err := runCmd(ctx, "ps", "-Ao", "pid=,pcpu=,rss=,user=,comm=", "--sort=-pcpu"); err == nil {
		sample.TopProcesses = ParseProcessList(out, topProcessLimit)
	}
	sample.TemperatureC = c.temperature()

	return sample, nil
}

// temperature reads the thermal zones the kernel exposes. Many VMs and some
// servers have none; that is "unknown", never zero.
func (c *linuxMetrics) temperature() *float32 {
	dirs, _ := filepath.Glob(c.path("sys/class/thermal/thermal_zone*"))
	zones := map[string]int64{}
	for _, dir := range dirs {
		kind := strings.TrimSpace(string(readFile(filepath.Join(dir, "type"))))
		milli, err := strconv.ParseInt(strings.TrimSpace(string(readFile(filepath.Join(dir, "temp")))), 10, 64)
		if kind == "" || err != nil {
			continue
		}
		if prev, seen := zones[kind]; !seen || milli > prev {
			zones[kind] = milli
		}
	}
	if value, ok := PickTemperature(zones); ok {
		return &value
	}
	return nil
}

func (c *linuxMetrics) path(rel string) string { return filepath.Join(c.root, rel) }
func (c *linuxMetrics) read(rel string) []byte { return readFile(c.path(rel)) }

func readFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// runCmd executes a command with a bound and returns its stdout.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("collect: %s: %w", name, err)
	}
	return out, nil
}
