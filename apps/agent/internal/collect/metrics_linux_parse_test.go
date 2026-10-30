package collect_test

import (
	"testing"

	"github.com/visionify/edgepilot/apps/agent/internal/collect"
)

func TestParseProcStatCPUAndPercents(t *testing.T) {
	before, err := collect.ParseProcStatCPU([]byte("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := collect.ParseProcStatCPU([]byte("cpu  160 0 70 900 70 0 0 0 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Deltas: user 60, system 20, idle+iowait 120, total 200.
	user, system, busy, ok := collect.CPUPercents(before, after)
	if !ok || user != 30 || system != 10 || busy != 40 {
		t.Errorf("got user=%v system=%v busy=%v ok=%v, want 30/10/40", user, system, busy, ok)
	}
	if _, _, _, ok := collect.CPUPercents(after, after); ok {
		t.Error("a zero-length interval must not produce a reading")
	}
}

func TestParseMemInfo(t *testing.T) {
	m, err := collect.ParseMemInfo([]byte("MemTotal:       16000000 kB\nMemFree:  1000 kB\nMemAvailable:   12000000 kB\nSwapTotal: 2000 kB\nSwapFree: 500 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 16000000*1024 || m.Used() != 4000000*1024 || m.SwapTotal-m.SwapFree != 1500*1024 {
		t.Errorf("got %+v used=%d", m, m.Used())
	}
	// Pre-3.14 kernel: no MemAvailable.
	old, err := collect.ParseMemInfo([]byte("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n"))
	if err != nil || old.Used() != 600*1024 {
		t.Errorf("old kernel used=%d err=%v, want %d", old.Used(), err, 600*1024)
	}
}

func TestParseProcNetDevSkipsVirtualInterfaces(t *testing.T) {
	raw := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999 1 0 0 0 0 0 0 9999 1 0 0 0 0 0 0
  eth0: 1000 1 0 0 0 0 0 0 2000 1 0 0 0 0 0 0
docker0: 5000 1 0 0 0 0 0 0 5000 1 0 0 0 0 0 0
vethab12: 7000 1 0 0 0 0 0 0 7000 1 0 0 0 0 0 0
 wlan0: 300 1 0 0 0 0 0 0 400 1 0 0 0 0 0 0
`
	rx, tx, err := collect.ParseProcNetDev([]byte(raw))
	if err != nil || rx != 1300 || tx != 2400 {
		t.Errorf("rx=%d tx=%d err=%v, want 1300/2400", rx, tx, err)
	}
}

func TestPickTemperature(t *testing.T) {
	if v, ok := collect.PickTemperature(map[string]int64{"acpitz": 90000, "x86_pkg_temp": 55000}); !ok || v != 55 {
		t.Errorf("package sensor should win, got %v %v", v, ok)
	}
	if v, ok := collect.PickTemperature(map[string]int64{"a": 40000, "b": 61500, "bogus": 255000}); !ok || v != 61.5 {
		t.Errorf("hottest plausible zone, got %v %v", v, ok)
	}
	if _, ok := collect.PickTemperature(map[string]int64{}); ok {
		t.Error("no zones must be unknown, not zero")
	}
}

func TestParseProcLoadAvg(t *testing.T) {
	l1, l5, l15, err := collect.ParseProcLoadAvg([]byte("0.52 1.50 2.25 1/467 12345\n"))
	if err != nil || l1 != 0.52 || l5 != 1.5 || l15 != 2.25 {
		t.Errorf("got %v %v %v %v", l1, l5, l15, err)
	}
}
