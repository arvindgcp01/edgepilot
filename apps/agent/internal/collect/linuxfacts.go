package collect

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// linuxCollector reads facts from a Linux machine.
//
// Everything comes from files — sysfs, procfs, os-release — rather than from
// dmidecode or lshw, which are not installed on a minimal image and whose
// output changes between versions. The files are the kernel's own interface and
// are the same on every distribution.
//
// The root is a field so the tests can point it at a fixture tree. In
// production it is "/".
type linuxCollector struct {
	root string
}

// NewLinux returns a collector that reads a Linux filesystem rooted at root.
//
// Untagged and exported so it runs, and is tested, on any OS: it reads files
// and nothing else. An empty root means "/".
func NewLinux(root string) Collector {
	if root == "" {
		root = "/"
	}
	return &linuxCollector{root: root}
}

func (c *linuxCollector) Collect(ctx context.Context) (*Facts, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	facts := &Facts{}

	facts.SerialNumber = c.serialNumber()
	if facts.SerialNumber == "" {
		return nil, ErrNoSerialNumber
	}
	facts.HardwareUUID = strings.ToUpper(cleanDMI(c.read("sys/class/dmi/id/product_uuid")))
	if isPlaceholderUUID(facts.HardwareUUID) {
		facts.HardwareUUID = ""
	}

	// Everything below is best-effort, as on a Mac: a machine that will not
	// report its CPU model is still a machine worth managing.
	facts.ModelName, facts.ModelIdentifier = c.model()
	facts.Chip = ParseCPUModel([]byte(c.read("proc/cpuinfo")))
	if facts.Chip == "" {
		facts.Chip = c.socFromDeviceTree()
	}
	facts.Arch = archForGOARCH(runtime.GOARCH)

	facts.CPUCores = len(cpuListFile(c.path("sys/devices/system/cpu/online")))
	facts.MemoryBytes = ParseMemTotal([]byte(c.read("proc/meminfo")))
	facts.DiskTotalBytes = diskTotalBytes(c.root)

	release := c.read("etc/os-release")
	if release == "" {
		// The spec's fallback location; /etc/os-release is usually a symlink to
		// it, but a stripped image can carry only one of the two.
		release = c.read("usr/lib/os-release")
	}
	facts.OSName, facts.OSVersion = ParseOSRelease([]byte(release))
	if facts.OSName == "" {
		facts.OSName = "Linux"
	}
	// The kernel release stands where macOS reports a build number: it is what
	// distinguishes two machines on the same distribution version.
	facts.OSBuild = strings.TrimSpace(c.read("proc/sys/kernel/osrelease"))

	if booted, err := ParseStatBootTime([]byte(c.read("proc/stat"))); err == nil {
		facts.BootedAt = booted
	}

	if name, err := os.Hostname(); err == nil {
		facts.Hostname = shortHostname(name)
	}
	facts.LocalIP = primaryLocalIP(ctx)

	return facts, nil
}

// serialNumber picks the most durable identifier the machine offers.
//
// Enrollment is idempotent on the serial number, so it has to survive a
// reinstall and differ between two machines. In order of preference:
//
//  1. The DMI system serial — what the vendor printed on the chassis.
//  2. The DMI board serial — present on white-box PCs that leave the system
//     serial as "To be filled by O.E.M.".
//  3. The device-tree serial — the SoC serial on ARM boards (Raspberry Pi,
//     Jetson), which have no DMI at all.
//  4. /etc/machine-id, hashed. This one does NOT survive a reinstall, and two
//     machines flashed from one golden image share it until it is regenerated.
//     It is the last resort precisely because of that; the MID- prefix makes it
//     recognisable in a device list.
//
// The DMI files are readable only by root, which the agent is.
func (c *linuxCollector) serialNumber() string {
	for _, rel := range []string{
		"sys/class/dmi/id/product_serial",
		"sys/class/dmi/id/board_serial",
		"sys/firmware/devicetree/base/serial-number",
		"proc/device-tree/serial-number",
	} {
		if serial := cleanDMI(c.read(rel)); serial != "" && !isPlaceholderSerial(serial) {
			return truncate(serial, 64)
		}
	}
	if id := strings.TrimSpace(c.read("etc/machine-id")); id != "" {
		return machineIDSerial(id)
	}
	return ""
}

// model returns a human name and an identifier.
//
// ARM boards describe themselves in the device tree ("Raspberry Pi 4 Model B
// Rev 1.4"); PCs do it through DMI, split across vendor and product.
func (c *linuxCollector) model() (name, identifier string) {
	if dt := cleanDMI(c.read("sys/firmware/devicetree/base/model")); dt != "" {
		name = dt
		if compatible := c.compatible(); len(compatible) > 0 {
			identifier = compatible[0]
		}
		return name, identifier
	}

	vendor := cleanDMI(c.read("sys/class/dmi/id/sys_vendor"))
	product := cleanDMI(c.read("sys/class/dmi/id/product_name"))
	if isPlaceholderSerial(vendor) {
		vendor = ""
	}
	if isPlaceholderSerial(product) {
		product = ""
	}
	switch {
	case vendor != "" && product != "" && !strings.HasPrefix(product, vendor):
		name = vendor + " " + product
	case product != "":
		name = product
	default:
		name = vendor
	}
	return name, product
}

// socFromDeviceTree names the SoC from the last "compatible" entry, which by
// convention is the most generic one — the chip rather than the board.
func (c *linuxCollector) socFromDeviceTree() string {
	compatible := c.compatible()
	if len(compatible) == 0 {
		return ""
	}
	return compatible[len(compatible)-1]
}

// compatible reads the device tree's NUL-separated "compatible" list.
func (c *linuxCollector) compatible() []string {
	raw := c.read("sys/firmware/devicetree/base/compatible")
	var out []string
	for part := range strings.SplitSeq(raw, "\x00") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c *linuxCollector) path(rel string) string { return filepath.Join(c.root, rel) }

// read returns a file's contents, or "" if it cannot be read. Every fact here is
// optional except the serial, and its absence is reported by the caller.
func (c *linuxCollector) read(rel string) string {
	raw, err := os.ReadFile(c.path(rel))
	if err != nil {
		return ""
	}
	return string(raw)
}

// --- parsers ---------------------------------------------------------------

// ParseOSRelease reads os-release(5): NAME and VERSION_ID.
//
//	NAME="Ubuntu"
//	VERSION_ID="24.04"
//
// Values may be double-quoted, single-quoted, or bare. VERSION_ID is absent on
// rolling distributions, which is fine — the kernel release still identifies
// the build.
func ParseOSRelease(raw []byte) (name, version string) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || strings.HasPrefix(key, "#") {
			continue
		}
		value = unquote(strings.TrimSpace(value))
		switch key {
		case "NAME":
			name = value
		case "VERSION_ID":
			version = value
		}
	}
	return name, version
}

// ParseCPUModel reads the processor name from /proc/cpuinfo.
//
// x86 reports "model name" per core; most arm64 kernels report no name at all,
// and older ARM kernels report "Hardware" instead. An empty result is normal on
// ARM, and the caller falls back to the device tree.
func ParseCPUModel(raw []byte) string {
	var hardware string
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.Join(strings.Fields(value), " ")
		switch strings.TrimSpace(key) {
		case "model name":
			if value != "" {
				return value
			}
		case "Hardware":
			if hardware == "" {
				hardware = value
			}
		}
	}
	return hardware
}

// ParseMemTotal reads MemTotal from /proc/meminfo, which is in kibibytes.
func ParseMemTotal(raw []byte) int64 {
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kib <= 0 {
			return 0
		}
		return kib << 10
	}
	return 0
}

// ParseStatBootTime reads the btime line from /proc/stat: seconds since the
// epoch, already in UTC, with no locale involved.
func ParseStatBootTime(raw []byte) (time.Time, error) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("collect: /proc/stat btime: %w", err)
		}
		if secs <= 0 {
			return time.Time{}, fmt.Errorf("collect: /proc/stat reports btime %d", secs)
		}
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, errors.New("collect: /proc/stat has no btime line")
}

// --- helpers ---------------------------------------------------------------

// cleanDMI trims a sysfs value. Device-tree strings are NUL-terminated, and
// DMI strings are padded with spaces by some firmware.
func cleanDMI(s string) string {
	return strings.TrimSpace(strings.TrimRight(s, "\x00\n"))
}

// placeholderSerials are what firmware vendors leave in DMI fields they never
// filled in. Every one of them is shared by thousands of machines, so taking one
// as a serial would make every such machine the same device.
var placeholderSerials = map[string]bool{
	"to be filled by o.e.m.":   true,
	"to be filled by oem":      true,
	"default string":           true,
	"system serial number":     true,
	"system product name":      true,
	"system manufacturer":      true,
	"not specified":            true,
	"not applicable":           true,
	"not available":            true,
	"none":                     true,
	"n/a":                      true,
	"na":                       true,
	"oem":                      true,
	"o.e.m.":                   true,
	"0":                        true,
	"00000000":                 true,
	"0123456789":               true,
	"123456789":                true,
	"1234567890":               true,
	"chassis serial number":    true,
	"base board serial number": true,
}

func isPlaceholderSerial(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || placeholderSerials[s] {
		return true
	}
	// A string of one repeated character — "0000000000", "FFFFFFFF", "........"
	// — is a blank field, not an identity.
	return strings.Count(s, s[:1]) == len(s)
}

// isPlaceholderUUID catches the all-zero, all-F, and sequential UUIDs that
// firmware reports when the field was never programmed.
func isPlaceholderUUID(u string) bool {
	hexOnly := strings.ReplaceAll(u, "-", "")
	if hexOnly == "" {
		return true
	}
	if strings.Count(hexOnly, hexOnly[:1]) == len(hexOnly) {
		return true
	}
	return strings.EqualFold(u, "03000200-0400-0500-0006-000700080009")
}

// machineIDSerial derives a serial from /etc/machine-id.
//
// Hashed with an application-specific key rather than sent verbatim, which is
// what machine-id(5) asks of any program that exposes it off the machine.
func machineIDSerial(id string) string {
	mac := hmac.New(sha256.New, []byte(strings.TrimSpace(id)))
	mac.Write([]byte("edgepilot-device-serial"))
	return "MID-" + strings.ToUpper(hex.EncodeToString(mac.Sum(nil)[:8]))
}

// archForGOARCH maps Go's architecture names onto the two the schema allows.
// The agent is built for exactly these two; anything else is left empty rather
// than wrong.
func archForGOARCH(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "arm64"
	default:
		return ""
	}
}

// cpuList reads a kernel CPU list file such as /sys/devices/system/cpu/online
// ("0-3,6"), returning the CPU numbers it names.
func cpuListFile(path string) []int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cpus []int
	for spec := range strings.SplitSeq(strings.TrimSpace(string(raw)), ",") {
		if spec == "" {
			continue
		}
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

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	// os-release permits backslash escapes inside double quotes; the only ones
	// that occur in practice are \" and \\.
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return string(bytes.ToValidUTF8([]byte(s[:n]), nil))
}
