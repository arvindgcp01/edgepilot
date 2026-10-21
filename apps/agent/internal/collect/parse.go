package collect

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// hardwareReport is the shape of `system_profiler -json SPHardwareDataType`.
//
// Every field is decoded as json.RawMessage or string deliberately: Apple
// changes types between hardware generations without changing the key, and a
// typed decode of the whole document fails wholesale when one field surprises
// it. Losing the chip name because the core count changed shape is not a trade
// worth making.
type hardwareReport struct {
	Items []hardwareItem `json:"SPHardwareDataType"`
}

type hardwareItem struct {
	MachineName     string          `json:"machine_name"`
	MachineModel    string          `json:"machine_model"`
	ChipType        string          `json:"chip_type"`
	CPUType         string          `json:"cpu_type"`
	SerialNumber    string          `json:"serial_number"`
	PlatformUUID    string          `json:"platform_UUID"`
	PhysicalMemory  string          `json:"physical_memory"`
	NumberProcessor json.RawMessage `json:"number_processors"`
	Packages        json.RawMessage `json:"packages"`
}

// ParseHardware reads `system_profiler -json SPHardwareDataType` into Facts.
//
// Fills only the hardware fields; the caller merges OS and network facts in.
func ParseHardware(raw []byte) (*Facts, error) {
	var report hardwareReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("collect: parse system_profiler hardware output: %w", err)
	}
	if len(report.Items) == 0 {
		return nil, fmt.Errorf("collect: system_profiler returned no hardware entry")
	}
	item := report.Items[0]

	facts := &Facts{
		ModelName:       item.MachineName,
		ModelIdentifier: item.MachineModel,
		SerialNumber:    strings.TrimSpace(item.SerialNumber),
		HardwareUUID:    strings.TrimSpace(item.PlatformUUID),
	}

	// Apple silicon reports chip_type; Intel reports cpu_type. Neither machine
	// reports both, and the server stores one column.
	facts.Chip = strings.TrimSpace(item.ChipType)
	if facts.Chip == "" {
		facts.Chip = strings.TrimSpace(item.CPUType)
	}

	facts.MemoryBytes = ParseMemorySize(item.PhysicalMemory)
	total, perf, eff := ParseProcessorCount(item.NumberProcessor)
	facts.CPUCores, facts.CPUPerformanceCores, facts.CPUEfficiencyCores = total, perf, eff

	// The architecture follows from the chip: an M-series Mac is arm64, and
	// anything Intel is x86_64. The server's schema constrains this to those two
	// values, so a guess is not acceptable — an unrecognised chip leaves it
	// empty rather than wrong.
	facts.Arch = archForChip(facts.Chip)

	return facts, nil
}

// ParseProcessorCount reads the `number_processors` field, which has two
// entirely different shapes.
//
// Apple silicon reports a string, "proc 10:6:4" — total, performance,
// efficiency. Intel reports a plain integer. Code that assumes either one
// crashes or silently reports zero cores on half the fleet.
func ParseProcessorCount(raw json.RawMessage) (total, performance, efficiency int) {
	if len(raw) == 0 {
		return 0, 0, 0
	}

	// Intel: a JSON number. Negatives are impossible on real hardware but
	// would be stored verbatim by the server, so they are clamped here rather
	// than trusted.
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		if n < 0 {
			return 0, 0, 0
		}
		return n, 0, 0
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, 0, 0
	}
	s = strings.TrimSpace(s)

	// Apple silicon: "proc 10:6:4".
	if rest, ok := strings.CutPrefix(s, "proc "); ok {
		parts := strings.Split(strings.TrimSpace(rest), ":")
		switch len(parts) {
		case 3:
			total = atoiOrZero(parts[0])
			performance = atoiOrZero(parts[1])
			efficiency = atoiOrZero(parts[2])
			return total, performance, efficiency
		case 1:
			return atoiOrZero(parts[0]), 0, 0
		default:
			return 0, 0, 0
		}
	}

	// Some builds report the count as a bare string.
	return atoiOrZero(s), 0, 0
}

// ParseMemorySize reads `physical_memory`, which is a human string like "16 GB".
//
// Apple uses decimal-prefixed units but binary quantities: a machine with
// 17,179,869,184 bytes of RAM reports "16 GB". Taking it literally as 16×10⁹
// would put every device's memory 7% low, and every "memory used %" derived from
// it 7% high.
func ParseMemorySize(s string) int64 {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return 0
	}

	value, err := strconv.ParseFloat(strings.ReplaceAll(fields[0], ",", ""), 64)
	if err != nil || value <= 0 {
		return 0
	}

	unit := ""
	if len(fields) > 1 {
		unit = strings.ToUpper(fields[1])
	}
	var multiplier float64
	switch unit {
	case "TB", "TIB":
		multiplier = 1 << 40
	case "GB", "GIB", "":
		multiplier = 1 << 30
	case "MB", "MIB":
		multiplier = 1 << 20
	case "KB", "KIB":
		multiplier = 1 << 10
	case "B", "BYTES":
		multiplier = 1
	default:
		return 0
	}
	return int64(value * multiplier)
}

// ParseSWVers reads the key/value output of `sw_vers`.
//
//	ProductName:		macOS
//	ProductVersion:		14.5
//	BuildVersion:		23F79
func ParseSWVers(raw []byte) (name, version, build string) {
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "ProductName":
			name = value
		case "ProductVersion":
			version = value
		case "BuildVersion":
			build = value
		}
	}
	return name, version, build
}

// ParseBootTime reads `sysctl -n kern.boottime`.
//
//	{ sec = 1717430400, usec = 123456 } Fri Jun  3 12:00:00 2024
//
// The seconds field is parsed rather than the trailing human date: the date is
// rendered in the machine's locale and timezone, and parsing localised dates is
// how an agent reports that a Mac booted in 1970.
func ParseBootTime(raw []byte) (time.Time, error) {
	s := string(raw)

	start := strings.Index(s, "sec = ")
	if start < 0 {
		return time.Time{}, fmt.Errorf("collect: kern.boottime has no sec field: %q", strings.TrimSpace(s))
	}
	rest := s[start+len("sec = "):]

	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	if end == 0 {
		return time.Time{}, fmt.Errorf("collect: kern.boottime sec is empty: %q", strings.TrimSpace(s))
	}

	secs, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("collect: kern.boottime sec: %w", err)
	}
	if secs <= 0 {
		return time.Time{}, fmt.Errorf("collect: kern.boottime reports %d", secs)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// archForChip maps a chip name onto the two architectures the schema allows.
func archForChip(chip string) string {
	switch {
	case chip == "":
		return ""
	case strings.HasPrefix(chip, "Apple M"), strings.EqualFold(chip, "Apple Silicon"):
		return "arm64"
	case strings.Contains(chip, "Intel"):
		return "x86_64"
	default:
		return ""
	}
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
