package collect_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/visionify/edgepilot/apps/agent/internal/collect"
)

// The Linux collector reads files, so these run against fixture trees in
// testdata/linux_* on any OS. See testdata/README.md for what each represents.

func collectLinux(t *testing.T, tree string) (*collect.Facts, error) {
	t.Helper()
	return collect.NewLinux(filepath.Join("testdata", tree)).Collect(context.Background())
}

func TestLinuxCollectorAcrossHardware(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tree string
		want collect.Facts
	}{
		{
			tree: "linux_nuc_ubuntu",
			want: collect.Facts{
				SerialNumber:    "G6TN12345678",
				HardwareUUID:    "5C1E2A4B-7D3F-4E8A-9B6C-0D1E2F3A4B5C",
				ModelName:       "Intel(R) Client Systems NUC11TNHi5",
				ModelIdentifier: "NUC11TNHi5",
				Chip:            "11th Gen Intel(R) Core(TM) i5-1135G7 @ 2.40GHz",
				CPUCores:        8,
				MemoryBytes:     16132468 << 10,
				OSName:          "Ubuntu", OSVersion: "24.04", OSBuild: "6.8.0-45-generic",
				BootedAt: time.Unix(1729500000, 0).UTC(),
			},
		},
		{
			// Every DMI field holds a firmware placeholder, and os-release is
			// only in /usr/lib. The serial has to fall through to machine-id
			// rather than enrolling as "To be filled by O.E.M." — which every
			// other board from the same vendor also reports.
			tree: "linux_whitebox_debian",
			want: collect.Facts{
				HardwareUUID: "", ModelName: "", ModelIdentifier: "",
				Chip:     "Intel(R) Celeron(R) N5105 @ 2.00GHz",
				CPUCores: 4, MemoryBytes: 8040296 << 10,
				OSName: "Debian GNU/Linux", OSVersion: "12", OSBuild: "6.1.0-26-amd64",
				BootedAt: time.Unix(1729400000, 0).UTC(),
			},
		},
		{
			// No DMI at all: identity, model, and SoC come from the device tree,
			// whose strings are NUL-terminated.
			tree: "linux_raspberrypi",
			want: collect.Facts{
				SerialNumber:    "10000000a1b2c3d4",
				ModelName:       "Raspberry Pi 4 Model B Rev 1.4",
				ModelIdentifier: "raspberrypi,4-model-b",
				Chip:            "brcm,bcm2711",
				CPUCores:        4,
				MemoryBytes:     3881660 << 10,
				OSName:          "Debian GNU/Linux", OSVersion: "12", OSBuild: "6.6.51+rpt-rpi-v8",
				BootedAt: time.Unix(1729300000, 0).UTC(),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.tree, func(t *testing.T) {
			t.Parallel()

			got, err := collectLinux(t, tc.tree)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if tc.want.SerialNumber != "" && got.SerialNumber != tc.want.SerialNumber {
				t.Errorf("SerialNumber = %q, want %q", got.SerialNumber, tc.want.SerialNumber)
			}
			check := map[string][2]string{
				"HardwareUUID":    {got.HardwareUUID, tc.want.HardwareUUID},
				"ModelName":       {got.ModelName, tc.want.ModelName},
				"ModelIdentifier": {got.ModelIdentifier, tc.want.ModelIdentifier},
				"Chip":            {got.Chip, tc.want.Chip},
				"OSName":          {got.OSName, tc.want.OSName},
				"OSVersion":       {got.OSVersion, tc.want.OSVersion},
				"OSBuild":         {got.OSBuild, tc.want.OSBuild},
			}
			for field, pair := range check {
				if pair[0] != pair[1] {
					t.Errorf("%s = %q, want %q", field, pair[0], pair[1])
				}
			}
			if got.CPUCores != tc.want.CPUCores {
				t.Errorf("CPUCores = %d, want %d", got.CPUCores, tc.want.CPUCores)
			}
			if got.MemoryBytes != tc.want.MemoryBytes {
				t.Errorf("MemoryBytes = %d, want %d", got.MemoryBytes, tc.want.MemoryBytes)
			}
			if !got.BootedAt.Equal(tc.want.BootedAt) {
				t.Errorf("BootedAt = %v, want %v", got.BootedAt, tc.want.BootedAt)
			}
			if got.Arch != "" && got.Arch != "arm64" && got.Arch != "x86_64" {
				t.Errorf("Arch = %q: the schema allows only arm64 and x86_64", got.Arch)
			}
		})
	}
}

// The machine-id fallback must be stable — enrollment is idempotent on it — and
// must not expose the raw machine-id, which machine-id(5) asks programs to
// treat as confidential.
func TestMachineIDSerialIsStableAndHashed(t *testing.T) {
	t.Parallel()

	first, err := collectLinux(t, "linux_whitebox_debian")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	second, err := collectLinux(t, "linux_whitebox_debian")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if first.SerialNumber != second.SerialNumber {
		t.Errorf("serial changed between runs: %q then %q", first.SerialNumber, second.SerialNumber)
	}
	if !strings.HasPrefix(first.SerialNumber, "MID-") {
		t.Errorf("serial = %q, want the MID- prefix that marks a machine-id fallback", first.SerialNumber)
	}
	if strings.Contains(strings.ToLower(first.SerialNumber), "0b9c8d7e") {
		t.Errorf("serial %q contains the raw machine-id", first.SerialNumber)
	}
	if l := len(first.SerialNumber); l > 64 {
		t.Errorf("serial is %d characters; the server allows 64", l)
	}
}

// A machine with no serial anywhere must fail loudly, exactly as a Mac does.
func TestLinuxCollectorWithNoIdentityRefusesToGuess(t *testing.T) {
	t.Parallel()

	_, err := collectLinux(t, "linux_no_identity")
	if !errors.Is(err, collect.ErrNoSerialNumber) {
		t.Fatalf("err = %v, want ErrNoSerialNumber", err)
	}
}

func TestParseOSRelease(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, raw, wantName, wantVersion string
	}{
		{"double quoted", "NAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"\n", "Ubuntu", "22.04"},
		{"bare", "NAME=Fedora\nVERSION_ID=40\n", "Fedora", "40"},
		{"single quoted", "NAME='Rocky Linux'\nVERSION_ID='9.4'\n", "Rocky Linux", "9.4"},
		{"rolling, no version", "NAME=\"Arch Linux\"\nBUILD_ID=rolling\n", "Arch Linux", ""},
		{"comments and blanks", "# generated\n\nNAME=\"Debian GNU/Linux\"\n", "Debian GNU/Linux", ""},
		{"escaped quote", `NAME="Say \"hi\""` + "\n", `Say "hi"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name, version := collect.ParseOSRelease([]byte(tc.raw))
			if name != tc.wantName || version != tc.wantVersion {
				t.Errorf("= (%q, %q), want (%q, %q)", name, version, tc.wantName, tc.wantVersion)
			}
		})
	}
}

func TestParseCPUModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, raw, want string
	}{
		{"x86", "processor\t: 0\nmodel name\t: AMD Ryzen 7  5800U\n", "AMD Ryzen 7 5800U"},
		{"older ARM kernel", "Processor\t: ARMv7\nHardware\t: BCM2835\n", "BCM2835"},
		{"arm64, no name", "processor\t: 0\nBogoMIPS\t: 108.00\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := collect.ParseCPUModel([]byte(tc.raw)); got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseMemTotal(t *testing.T) {
	t.Parallel()

	if got := collect.ParseMemTotal([]byte("MemTotal:       16384 kB\n")); got != 16384<<10 {
		t.Errorf("= %d, want %d", got, 16384<<10)
	}
	for _, raw := range []string{"", "MemFree: 10 kB\n", "MemTotal: garbage kB\n", "MemTotal: -5 kB\n"} {
		if got := collect.ParseMemTotal([]byte(raw)); got != 0 {
			t.Errorf("ParseMemTotal(%q) = %d, want 0", raw, got)
		}
	}
}

func TestParseStatBootTime(t *testing.T) {
	t.Parallel()

	got, err := collect.ParseStatBootTime([]byte("cpu 1 2 3\nbtime 1700000000\n"))
	if err != nil {
		t.Fatalf("ParseStatBootTime: %v", err)
	}
	if want := time.Unix(1700000000, 0).UTC(); !got.Equal(want) {
		t.Errorf("= %v, want %v", got, want)
	}
	for _, raw := range []string{"", "cpu 1 2 3\n", "btime 0\n", "btime soon\n"} {
		if _, err := collect.ParseStatBootTime([]byte(raw)); err == nil {
			t.Errorf("ParseStatBootTime(%q) succeeded, want an error", raw)
		}
	}
}
