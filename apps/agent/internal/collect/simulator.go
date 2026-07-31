//go:build !darwin || simulator

package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// simulatedCollector stands in for a Mac.
//
// # Why this exists
//
// CI runs Linux. Without this, everything downstream of collection — enrolment,
// the handshake, inventory reporting, the server's device table — could only be
// tested against hand-written fixtures, which test the fixtures rather than the
// path.
//
// # What it guarantees
//
// The facts are *stable for a given machine*: the serial number and hardware
// UUID are derived from the host's own hostname, so a simulated device keeps its
// identity across restarts. Enrolment is idempotent on the serial number, and a
// collector that invented a fresh one each run would register a new device on
// every restart and make the idempotency untestable.
//
// # What it is not
//
// It is not a fake for unit tests to assert against. It is a real Collector that
// reports real facts about the machine it runs on, with the Mac-specific ones
// synthesised and clearly marked as such: the model name is
// "EdgePilot Simulator" and the serial number carries a SIM- prefix, so a
// simulated device is never mistaken for hardware in a device list.
type simulatedCollector struct{}

func newCollector() Collector { return &simulatedCollector{} }

// simulatorNamespace derives stable identifiers from an unstable input.
var simulatorNamespace = uuid.MustParse("6f3f1e8c-9b7a-7c4e-8f1d-3a5c7e9b1d2f")

func (c *simulatedCollector) Collect(ctx context.Context) (*Facts, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		// A machine with no hostname would produce a different identity on
		// every run, which breaks the one property this collector must keep.
		return nil, fmt.Errorf("collect: the simulator needs a hostname: %w", err)
	}
	hostname = shortHostname(hostname)

	facts := &Facts{
		Hostname:            hostname,
		SerialNumber:        simulatedSerial(hostname),
		HardwareUUID:        uuid.NewSHA1(simulatorNamespace, []byte(hostname)).String(),
		ModelName:           "EdgePilot Simulator",
		ModelIdentifier:     "Simulator1,1",
		Chip:                "Apple M2 Pro",
		Arch:                "arm64",
		CPUCores:            runtimeCores(),
		CPUPerformanceCores: runtimeCores(),
		MemoryBytes:         simulatedMemoryBytes(),
		DiskTotalBytes:      diskTotalBytes("/"),
		OSName:              "macOS",
		OSVersion:           "15.0",
		OSBuild:             "SIMULATOR",
		LocalIP:             primaryLocalIP(ctx),
		BootedAt:            processStart(),
	}
	return facts, nil
}

// simulatedSerial produces a stable, obviously-synthetic serial number.
//
// Apple serials are 10 or 12 characters; this is 12 with a SIM- prefix so it is
// unmistakable in a device list, and it still fits the server's 64-character
// limit.
func simulatedSerial(hostname string) string {
	sum := sha256.Sum256([]byte("edgepilot-simulator:" + hostname))
	return "SIM-" + strings.ToUpper(hex.EncodeToString(sum[:4]))
}

func runtimeCores() int {
	n := len(cpuList())
	if n == 0 {
		return 1
	}
	return n
}

// cpuList reads the online CPUs, so the simulated core count matches the
// machine the tests actually run on.
func cpuList() []int {
	raw, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil
	}
	var cpus []int
	for spec := range strings.SplitSeq(strings.TrimSpace(string(raw)), ",") {
		lo, hi, found := strings.Cut(spec, "-")
		start := atoiOrZero(lo)
		end := start
		if found {
			end = atoiOrZero(hi)
		}
		for i := start; i <= end && i-start < 4096; i++ {
			cpus = append(cpus, i)
		}
	}
	return cpus
}

// simulatedMemoryBytes reports the host's real memory where it can be read,
// falling back to a plausible Mac mini configuration.
//
// Read from /proc rather than through syscall.Sysinfo, which is Linux-only:
// this file also builds under `simulator` on a developer's Mac, where the
// fallback is what runs.
func simulatedMemoryBytes() int64 {
	const fallback = 16 << 30

	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return fallback
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			break
		}
		// MemTotal is in kibibytes.
		if kib := atoiOrZero(fields[0]); kib > 0 {
			return int64(kib) << 10
		}
		break
	}
	return fallback
}

// processStart approximates a boot time.
//
// The agent's own start, not the host's: a simulated device that claims the
// CI runner's uptime would report months of uptime for a container that has
// existed for seconds.
func processStart() time.Time { return startedAt }

var startedAt = time.Now().UTC()
