package collect

import (
	"testing"
)

// Captured from real machines. Every case is a formatting quirk that has
// actually shipped in a macOS release, which is the only reason to keep the
// sample verbatim rather than tidy it.

func TestParseTopCPUTakesTheSecondSample(t *testing.T) {
	raw := []byte(`Processes: 512 total, 3 running, 509 sleeping, 2534 threads
2026/09/04 10:11:12
Load Avg: 1.52, 1.68, 1.75
CPU usage: 12.5% user, 25.0% sys, 62.5% idle
SharedLibs: 400M resident, 60M data, 40M linkedit.
Processes: 512 total, 2 running, 510 sleeping, 2530 threads
CPU usage: 3.57% user, 7.14% sys, 89.28% idle
`)
	user, sys, idle, err := ParseTopCPU(raw)
	if err != nil {
		t.Fatal(err)
	}
	if user != 3.57 || sys != 7.14 || idle != 89.28 {
		t.Errorf("got user=%v sys=%v idle=%v, want the second sample 3.57/7.14/89.28", user, sys, idle)
	}
}

func TestParseTopCPURejectsOutputWithoutTheLine(t *testing.T) {
	if _, _, _, err := ParseTopCPU([]byte("Processes: 1 total\n")); err == nil {
		t.Error("expected an error for output without a CPU usage line")
	}
}

func TestParseVMStatUsesThePageSizeInTheHeader(t *testing.T) {
	raw := []byte(`Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               10000.
Pages active:                             20000.
Pages inactive:                           15000.
Pages speculative:                         1000.
Pages throttled:                              0.
Pages wired down:                          5000.
Pages purgeable:                            200.
"Translation faults":                  12345678.
Pages occupied by compressor:              3000.
`)
	stat, err := ParseVMStat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stat.PageSize != 16384 {
		t.Errorf("page size = %d, want 16384", stat.PageSize)
	}
	// active + wired + compressor = 28,000 pages of 16 KiB.
	if want := int64(28000 * 16384); stat.Used() != want {
		t.Errorf("used = %d, want %d", stat.Used(), want)
	}
	if stat.Free != 10000*16384 {
		t.Errorf("free = %d, want %d", stat.Free, 10000*16384)
	}
}

func TestParseMemoryPressureInvertsFree(t *testing.T) {
	raw := []byte(`The system has 2147483648 (524288 pages with a page size of 4096).

Stats:
Pages free: 12345
...
System-wide memory free percentage: 62%
`)
	pressure, err := ParseMemoryPressure(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pressure != 38 {
		t.Errorf("pressure = %v, want 38", pressure)
	}
}

func TestParseSwapUsage(t *testing.T) {
	used, err := ParseSwapUsage([]byte("total = 2048.00M  used = 1136.25M  free = 911.75M  (encrypted)\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1136.25 * (1 << 20)); used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
	if used, err := ParseSwapUsage([]byte("total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)")); err != nil || used != 0 {
		t.Errorf("zero swap: used=%d err=%v", used, err)
	}
}

func TestParseLoadAvg(t *testing.T) {
	l1, l5, l15, err := ParseLoadAvg([]byte("{ 1.52 1.68 1.75 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l1 != 1.52 || l5 != 1.68 || l15 != 1.75 {
		t.Errorf("got %v %v %v", l1, l5, l15)
	}
}

func TestParseNetstatIBSumsPhysicalInterfacesOnly(t *testing.T) {
	raw := []byte(`Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                          1000     0     100000     1000     0     100000     0
lo0        16384 127           127.0.0.1           1000     -     100000     1000     -     100000     -
gif0*      1280  <Link#2>                             0     0          0        0     0          0     0
en0        1500  <Link#4>      aa:bb:cc:dd:ee:ff  50000     0    5000000    30000     0    3000000     0
en0        1500  192.168.1     192.168.1.20       50000     -    5000000    30000     -    3000000     -
en1        1500  <Link#5>      11:22:33:44:55:66    200     0      20000      100     0      10000     0
`)
	rx, tx, err := ParseNetstatIB(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rx != 5020000 || tx != 3010000 {
		t.Errorf("rx=%d tx=%d, want 5020000/3010000 (loopback and gif excluded, addresses not double counted)", rx, tx)
	}
}

func TestParseTemperature(t *testing.T) {
	if v, ok := ParseTemperature([]byte("**** SMC sensors ****\n\nCPU die temperature: 61.13 C\n")); !ok || v != 61.13 {
		t.Errorf("got %v %v, want 61.13 true", v, ok)
	}
	if _, ok := ParseTemperature([]byte("powermetrics: unrecognized sampler: smc\n")); ok {
		t.Error("Apple silicon output must report no sensor, not a value")
	}
}

func TestParseProcessListKeepsTheHighestAndStripsThePath(t *testing.T) {
	raw := []byte(`  501  45.2  102400 alice  /Applications/Final Cut Pro.app/Contents/MacOS/Final Cut Pro
  123  12.0   51200 root   /usr/libexec/edgepilot-agent
    1   0.1    8192 root   /sbin/launchd
`)
	procs := ParseProcessList(raw, 2)
	if len(procs) != 2 {
		t.Fatalf("got %d processes, want 2", len(procs))
	}
	if procs[0].Name != "Final Cut Pro" || procs[0].PID != 501 || procs[0].MemoryBytes != 102400*1024 {
		t.Errorf("first = %+v", procs[0])
	}
	if procs[1].Name != "edgepilot-agent" || procs[1].User != "root" {
		t.Errorf("second = %+v", procs[1])
	}
}
