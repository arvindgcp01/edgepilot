// Package collect gathers the facts the server keeps about a Mac.
//
// # Why the parsing is separated from the collecting
//
// Reading facts off a Mac means shelling out to system_profiler, sw_vers, and
// sysctl. None of those exist in CI, which runs Linux, and none of them can be
// made to return a Mac Studio's output on demand.
//
// So the package is split in two. Running the commands is platform code behind a
// build tag. Turning their output into Facts is ordinary Go with no platform
// dependency at all, and it is tested against captured output in testdata/ —
// including the shapes that differ between Apple silicon and Intel, which is
// where this kind of code actually breaks.
//
// The `simulator` build tag swaps the platform half for one that synthesises
// plausible facts, so the whole control-plane path — enrol, connect, report — is
// exercisable on Linux.
package collect

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

// Facts is what the server stores about a device's hardware and OS.
//
// Fields are best-effort: a Mac that will not report its model identifier is
// still a Mac worth managing, so a missing fact is an empty field rather than a
// failed collection. Only the serial number is genuinely required, because it is
// what makes enrollment idempotent.
type Facts struct {
	Hostname     string
	SerialNumber string
	HardwareUUID string

	ModelIdentifier string
	ModelName       string
	Chip            string
	Arch            string

	CPUCores            int
	CPUPerformanceCores int
	CPUEfficiencyCores  int

	MemoryBytes    int64
	DiskTotalBytes int64

	OSName    string
	OSVersion string
	OSBuild   string

	LocalIP  string
	BootedAt time.Time
}

// ErrNoSerialNumber reports that the machine did not yield a serial number.
//
// Fatal for enrollment specifically: the serial is what makes enrollment
// idempotent, so without it a retrying agent would register a second device
// every time.
var ErrNoSerialNumber = errors.New("collect: this machine reports no serial number")

// Uptime returns how long the machine has been up.
func (f *Facts) Uptime(now time.Time) time.Duration {
	if f == nil || f.BootedAt.IsZero() {
		return 0
	}
	d := now.Sub(f.BootedAt)
	if d < 0 {
		return 0
	}
	return d
}

// Collector gathers facts from the machine.
//
// An interface because there are two implementations — the real one and the
// simulator — and because it is what a test substitutes to drive the connection
// manager without a Mac.
type Collector interface {
	Collect(ctx context.Context) (*Facts, error)
}

// New returns the collector for this build.
//
// Defined per platform: darwin.go for a real Mac, simulator.go everywhere else
// and under the `simulator` tag.
func New() Collector { return newCollector() }

// primaryLocalIP returns the address the machine would use to reach the network.
//
// A UDP "connection" to a routable address, which sends nothing but makes the
// kernel pick a source address — this is the interface the server would see, and
// it beats guessing at en0 or picking the first non-loopback address on a Mac
// with VPNs and virtual interfaces.
func primaryLocalIP(ctx context.Context) string {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	// TEST-NET-1: reserved for documentation and never routed, so nothing
	// leaves the machine — the point is the kernel's choice of source address,
	// not the packet.
	conn, err := dialer.DialContext(ctx, "udp4", "192.0.2.1:9")
	if err != nil {
		return fallbackLocalIP()
	}
	defer func() { _ = conn.Close() }()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return fallbackLocalIP()
	}
	return addr.IP.String()
}

// fallbackLocalIP picks the first non-loopback IPv4 address, for a machine with
// no default route — an air-gapped site, or a laptop with the network down.
func fallbackLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// shortHostname strips the trailing dot and any domain, matching what a Mac
// shows in Sharing preferences rather than the FQDN a DHCP server invented.
func shortHostname(name string) string {
	name = strings.TrimSuffix(name, ".")
	if i := strings.Index(name, "."); i > 0 {
		// Keep .local names whole: they are the machine's own identity on the
		// LAN, and truncating them loses the distinction between two devices
		// with the same short name.
		if !strings.HasSuffix(name, ".local") {
			return name[:i]
		}
	}
	return name
}
