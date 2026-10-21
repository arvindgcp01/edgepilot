package collect_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/visionify/edgepilot/apps/agent/internal/collect"
)

// These run on Linux, which is the point: the parsing is where this code breaks,
// and it breaks on hardware the CI machine will never be. See testdata/README.md
// for what each fixture represents.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// --- hardware --------------------------------------------------------------

func TestParseHardwareAcrossTheFleet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fixture string
		want    collect.Facts
	}{
		{
			fixture: "hardware_m2pro_macmini.json",
			want: collect.Facts{
				ModelName: "Mac mini", ModelIdentifier: "Mac14,12",
				Chip: "Apple M2 Pro", Arch: "arm64",
				SerialNumber: "H2WK9J4YQ1",
				HardwareUUID: "F1B2C3D4-5E6F-4A7B-8C9D-0E1F2A3B4C5D",
				CPUCores:     12, CPUPerformanceCores: 8, CPUEfficiencyCores: 4,
				MemoryBytes: 32 << 30,
			},
		},
		{
			fixture: "hardware_m2ultra_macstudio.json",
			want: collect.Facts{
				ModelName: "Mac Studio", ModelIdentifier: "Mac14,14",
				Chip: "Apple M2 Ultra", Arch: "arm64",
				SerialNumber: "J5XQ8M2NP7VK",
				HardwareUUID: "A9B8C7D6-E5F4-4321-9876-543210FEDCBA",
				CPUCores:     24, CPUPerformanceCores: 16, CPUEfficiencyCores: 8,
				MemoryBytes: 192 << 30,
			},
		},
		{
			// The one that catches the Apple-silicon assumption: an integer
			// core count and the chip under a different key.
			fixture: "hardware_intel_macmini.json",
			want: collect.Facts{
				ModelName: "Mac mini", ModelIdentifier: "Macmini8,1",
				Chip: "Intel Core i7", Arch: "x86_64",
				SerialNumber: "C07YR2ZWJYVX",
				HardwareUUID: "1A2B3C4D-5E6F-5A7B-9C8D-1E2F3A4B5C6D",
				CPUCores:     6,
				MemoryBytes:  16 << 30,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			got, err := collect.ParseHardware(fixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if got.ModelName != tc.want.ModelName {
				t.Errorf("model name = %q, want %q", got.ModelName, tc.want.ModelName)
			}
			if got.ModelIdentifier != tc.want.ModelIdentifier {
				t.Errorf("model identifier = %q, want %q", got.ModelIdentifier, tc.want.ModelIdentifier)
			}
			if got.Chip != tc.want.Chip {
				t.Errorf("chip = %q, want %q", got.Chip, tc.want.Chip)
			}
			if got.Arch != tc.want.Arch {
				t.Errorf("arch = %q, want %q", got.Arch, tc.want.Arch)
			}
			if got.SerialNumber != tc.want.SerialNumber {
				t.Errorf("serial = %q, want %q", got.SerialNumber, tc.want.SerialNumber)
			}
			if got.HardwareUUID != tc.want.HardwareUUID {
				t.Errorf("hardware uuid = %q, want %q", got.HardwareUUID, tc.want.HardwareUUID)
			}
			if got.CPUCores != tc.want.CPUCores {
				t.Errorf("cores = %d, want %d", got.CPUCores, tc.want.CPUCores)
			}
			if got.CPUPerformanceCores != tc.want.CPUPerformanceCores {
				t.Errorf("performance cores = %d, want %d",
					got.CPUPerformanceCores, tc.want.CPUPerformanceCores)
			}
			if got.CPUEfficiencyCores != tc.want.CPUEfficiencyCores {
				t.Errorf("efficiency cores = %d, want %d",
					got.CPUEfficiencyCores, tc.want.CPUEfficiencyCores)
			}
			if got.MemoryBytes != tc.want.MemoryBytes {
				t.Errorf("memory = %d, want %d", got.MemoryBytes, tc.want.MemoryBytes)
			}
		})
	}
}

// The architecture column is constrained to arm64 and x86_64. An unrecognised
// chip must leave it empty rather than guess, because a wrong value would be
// rejected by the server and a guessed one would be believed.
func TestArchIsDerivedRatherThanGuessed(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`{"SPHardwareDataType":[{"chip_type":"Apple M4 Max","serial_number":"X"}]}`:        "arm64",
		`{"SPHardwareDataType":[{"cpu_type":"Intel Core i9","serial_number":"X"}]}`:        "x86_64",
		`{"SPHardwareDataType":[{"cpu_type":"Quad-Core Intel Xeon","serial_number":"X"}]}`: "x86_64",
		`{"SPHardwareDataType":[{"chip_type":"Something New","serial_number":"X"}]}`:       "",
		`{"SPHardwareDataType":[{"serial_number":"X"}]}`:                                   "",
	}

	for raw, want := range cases {
		got, err := collect.ParseHardware([]byte(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if got.Arch != want {
			t.Errorf("arch for %s = %q, want %q", raw, got.Arch, want)
		}
	}
}

// Enrollment is idempotent on the serial number. Without one, a retrying agent
// registers a second device every time — so this has to be visible to the
// caller rather than papered over with an empty string.
func TestAMachineWithNoSerialIsRecognisable(t *testing.T) {
	t.Parallel()

	got, err := collect.ParseHardware(fixture(t, "hardware_no_serial.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.SerialNumber != "" {
		t.Errorf("serial = %q, want empty", got.SerialNumber)
	}
	// The rest still parses: the caller decides that a missing serial is fatal,
	// not the parser.
	if got.ModelIdentifier != "Mac14,3" {
		t.Errorf("model = %q, want Mac14,3", got.ModelIdentifier)
	}
}

func TestParseHardwareRejectsUnusableOutput(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		``,
		`not json`,
		`{"SPHardwareDataType":[]}`,
		`{}`,
	} {
		if _, err := collect.ParseHardware([]byte(raw)); err == nil {
			t.Errorf("parsing %q should have failed", raw)
		}
	}
}

// --- the two shapes of number_processors -----------------------------------

func TestProcessorCountHandlesBothHardwareGenerations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                    string
		raw                     string
		total, perf, efficiency int
	}{
		{"apple silicon", `"proc 12:8:4"`, 12, 8, 4},
		{"apple silicon, all performance", `"proc 8:8:0"`, 8, 8, 0},
		{"intel, an integer", `10`, 10, 0, 0},
		{"a bare string count", `"4"`, 4, 0, 0},
		{"absent", ``, 0, 0, 0},
		{"unparseable", `"proc weird"`, 0, 0, 0},
		{"wrong shape", `{"total":8}`, 0, 0, 0},
		{"a negative count is clamped rather than stored", `-4`, 0, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			total, perf, eff := collect.ParseProcessorCount(json.RawMessage(tc.raw))
			if total != tc.total || perf != tc.perf || eff != tc.efficiency {
				t.Errorf("got (%d, %d, %d), want (%d, %d, %d)",
					total, perf, eff, tc.total, tc.perf, tc.efficiency)
			}
		})
	}
}

// --- memory ----------------------------------------------------------------

// Apple prints decimal-looking units for binary quantities: a machine with
// 17,179,869,184 bytes reports "16 GB". Reading that as 16×10⁹ puts every
// device's memory 7% low and every derived percentage 7% high.
func TestMemorySizesAreBinaryDespiteTheUnits(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{
		"8 GB":    8 << 30,
		"16 GB":   16 << 30,
		"192 GB":  192 << 30,
		"1 TB":    1 << 40,
		"512 MB":  512 << 20,
		"1,024 M": 0, // an unknown unit is refused rather than guessed
		"":        0,
		"lots":    0,
		"0 GB":    0,
		"-8 GB":   0,
	}

	for input, want := range cases {
		if got := collect.ParseMemorySize(input); got != want {
			t.Errorf("ParseMemorySize(%q) = %d, want %d", input, got, want)
		}
	}
}

// --- the OS ----------------------------------------------------------------

func TestParseSWVers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fixture              string
		name, version, build string
	}{
		{"sw_vers_sonoma.txt", "macOS", "14.5", "23F79"},
		{"sw_vers_sequoia.txt", "macOS", "15.1", "24B83"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			name, version, build := collect.ParseSWVers(fixture(t, tc.fixture))
			if name != tc.name || version != tc.version || build != tc.build {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					name, version, build, tc.name, tc.version, tc.build)
			}
		})
	}
}

func TestParseSWVersToleratesJunk(t *testing.T) {
	t.Parallel()

	name, version, build := collect.ParseSWVers([]byte("garbage\nwith no colons\n"))
	if name != "" || version != "" || build != "" {
		t.Errorf("got (%q, %q, %q), want all empty", name, version, build)
	}
}

// --- boot time -------------------------------------------------------------

// The seconds field is parsed, not the trailing date: that date is rendered in
// the machine's locale and timezone, and parsing localised dates is how an agent
// ends up reporting that a Mac booted in 1970.
func TestParseBootTimeReadsTheEpochSeconds(t *testing.T) {
	t.Parallel()

	got, err := collect.ParseBootTime(fixture(t, "boottime.txt"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := time.Unix(1717430400, 0).UTC()
	if !got.Equal(want) {
		t.Errorf("boot time = %s, want %s", got, want)
	}
}

func TestParseBootTimeIgnoresTheLocalisedDate(t *testing.T) {
	t.Parallel()

	// The same instant, with the trailing date rendered in German — which is
	// what a Mac set to de_DE prints.
	raw := []byte("{ sec = 1717430400, usec = 4711 } Mo  3 Jun 12:00:00 2024\n")
	got, err := collect.ParseBootTime(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Equal(time.Unix(1717430400, 0).UTC()) {
		t.Errorf("boot time = %s, want the epoch seconds", got)
	}
}

func TestParseBootTimeRejectsNonsense(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"no boottime here",
		"{ sec = , usec = 0 }",
		"{ sec = 0, usec = 0 }",
		"{ sec = -5, usec = 0 }",
	} {
		if _, err := collect.ParseBootTime([]byte(raw)); err == nil {
			t.Errorf("parsing %q should have failed", raw)
		}
	}
}

// --- uptime ----------------------------------------------------------------

func TestUptimeIsNeverNegative(t *testing.T) {
	t.Parallel()

	booted := time.Now().Add(-3 * time.Hour)
	facts := &collect.Facts{BootedAt: booted}

	if got := facts.Uptime(booted.Add(90 * time.Minute)); got != 90*time.Minute {
		t.Errorf("uptime = %s, want 90m", got)
	}
	// Clock skew, or a boot time read after an NTP step.
	if got := facts.Uptime(booted.Add(-time.Hour)); got != 0 {
		t.Errorf("uptime = %s for a clock before boot, want 0", got)
	}
	if got := (&collect.Facts{}).Uptime(time.Now()); got != 0 {
		t.Errorf("uptime without a boot time = %s, want 0", got)
	}
	var nilFacts *collect.Facts
	if got := nilFacts.Uptime(time.Now()); got != 0 {
		t.Errorf("uptime of no facts = %s, want 0", got)
	}
}
