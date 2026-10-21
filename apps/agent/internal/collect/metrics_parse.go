package collect

import (
	"bufio"
	"errors"
	"strconv"
	"strings"
)

// The parsers for the metrics collector. Pure functions over command output,
// so they are testable on any platform from captured samples of the real
// thing — which is where every macOS version's formatting quirk gets caught.

// ParseTopCPU reads the last "CPU usage" line top prints:
//
//	CPU usage: 3.57% user, 7.14% sys, 89.28% idle
//
// top -l 2 prints two; the first is since boot and meaningless, the last is
// over the sampling interval, which is why the last line wins.
func ParseTopCPU(raw []byte) (user, system, idle float32, err error) {
	var line string
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "CPU usage:") {
			line = scanner.Text()
		}
	}
	if line == "" {
		return 0, 0, 0, errors.New("collect: top printed no CPU usage line")
	}
	found := 0
	for _, field := range strings.Split(strings.TrimPrefix(line, "CPU usage:"), ",") {
		parts := strings.Fields(field)
		if len(parts) != 2 {
			continue
		}
		value, convErr := strconv.ParseFloat(strings.TrimSuffix(parts[0], "%"), 32)
		if convErr != nil {
			continue
		}
		switch parts[1] {
		case "user":
			user = float32(value)
			found++
		case "sys":
			system = float32(value)
			found++
		case "idle":
			idle = float32(value)
			found++
		}
	}
	if found < 3 {
		return 0, 0, 0, errors.New("collect: could not parse top's CPU usage line: " + line)
	}
	return user, system, idle, nil
}

// VMStat is what vm_stat reports, in bytes rather than pages.
type VMStat struct {
	PageSize   int64
	Free       int64
	Active     int64
	Inactive   int64
	Wired      int64
	Compressed int64
	// Speculative pages are file cache the kernel may reclaim at will; they
	// count as free for the purpose of "how much memory is this Mac using".
	Speculative int64
}

// Used is what Activity Monitor calls "Memory Used": app memory, wired memory,
// and the compressor. Inactive and speculative pages are reclaimable and are
// not counted, because a Mac with a warm file cache is not a Mac under load.
func (v VMStat) Used() int64 { return v.Active + v.Wired + v.Compressed }

// ParseVMStat reads vm_stat's output:
//
//	Mach Virtual Memory Statistics: (page size of 16384 bytes)
//	Pages free:                               12345.
//	Pages active:                            123456.
//	...
//	Pages occupied by compressor:             54321.
func ParseVMStat(raw []byte) (VMStat, error) {
	stat := VMStat{PageSize: 4096}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	seen := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := strings.Fields(line[i+len("page size of "):])
				if len(rest) > 0 {
					if size, err := strconv.ParseInt(rest[0], 10, 64); err == nil && size > 0 {
						stat.PageSize = size
					}
				}
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		pages, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			continue
		}
		bytes := pages * stat.PageSize
		switch strings.TrimSpace(key) {
		case "Pages free":
			stat.Free = bytes
			seen++
		case "Pages active":
			stat.Active = bytes
			seen++
		case "Pages inactive":
			stat.Inactive = bytes
			seen++
		case "Pages wired down":
			stat.Wired = bytes
			seen++
		case "Pages occupied by compressor":
			stat.Compressed = bytes
			seen++
		case "Pages speculative":
			stat.Speculative = bytes
		}
	}
	if seen < 4 {
		return VMStat{}, errors.New("collect: vm_stat output did not contain the expected counters")
	}
	return stat, nil
}

// ParseMemoryPressure reads the free percentage memory_pressure prints and
// returns it as pressure: 100 minus free.
//
//	System-wide memory free percentage: 62%
func ParseMemoryPressure(raw []byte) (float32, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "System-wide memory free percentage:") {
			continue
		}
		value := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, "System-wide memory free percentage:")), "%")
		free, err := strconv.ParseFloat(value, 32)
		if err != nil {
			return 0, errors.New("collect: could not parse memory_pressure: " + line)
		}
		return float32(100 - free), nil
	}
	return 0, errors.New("collect: memory_pressure printed no free percentage")
}

// ParseSwapUsage reads vm.swapusage:
//
//	total = 2048.00M  used = 1136.25M  free = 911.75M  (encrypted)
func ParseSwapUsage(raw []byte) (used int64, err error) {
	fields := strings.Fields(string(raw))
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] == "used" && fields[i+1] == "=" {
			return parseSizeWithSuffix(fields[i+2])
		}
	}
	return 0, errors.New("collect: could not parse vm.swapusage: " + strings.TrimSpace(string(raw)))
}

func parseSizeWithSuffix(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("collect: empty size")
	}
	multiplier := int64(1)
	switch s[len(s)-1] {
	case 'K':
		multiplier = 1 << 10
	case 'M':
		multiplier = 1 << 20
	case 'G':
		multiplier = 1 << 30
	case 'T':
		multiplier = 1 << 40
	}
	if multiplier != 1 {
		s = s[:len(s)-1]
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(value * float64(multiplier)), nil
}

// ParseLoadAvg reads vm.loadavg:
//
//	{ 1.52 1.68 1.75 }
func ParseLoadAvg(raw []byte) (load1, load5, load15 float32, err error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(raw)), "{}"))
	if len(fields) < 3 {
		return 0, 0, 0, errors.New("collect: could not parse vm.loadavg: " + strings.TrimSpace(string(raw)))
	}
	values := make([]float32, 3)
	for i := range 3 {
		v, convErr := strconv.ParseFloat(fields[i], 32)
		if convErr != nil {
			return 0, 0, 0, errors.New("collect: could not parse vm.loadavg: " + strings.TrimSpace(string(raw)))
		}
		values[i] = float32(v)
	}
	return values[0], values[1], values[2], nil
}

// ParseNetstatIB sums received and sent bytes over every physical interface.
//
// netstat -ibn prints one line per address per interface; the line whose
// Network column is "<Link#N>" carries the interface's own counters and is
// the only one counted. Loopback is skipped: traffic to itself is not
// traffic. The columns are found by header name because their positions
// differ between macOS versions.
func ParseNetstatIB(raw []byte) (rx, tx int64, err error) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	if !scanner.Scan() {
		return 0, 0, errors.New("collect: netstat printed nothing")
	}
	header := strings.Fields(scanner.Text())
	ibytes, obytes, network := -1, -1, -1
	for i, name := range header {
		switch name {
		case "Ibytes":
			ibytes = i
		case "Obytes":
			obytes = i
		case "Network":
			network = i
		}
	}
	if ibytes < 0 || obytes < 0 || network < 0 {
		return 0, 0, errors.New("collect: netstat header lacks Ibytes/Obytes columns")
	}
	counted := 0
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) <= obytes || len(fields) <= ibytes {
			continue
		}
		if !strings.HasPrefix(fields[network], "<Link#") {
			continue
		}
		name := fields[0]
		if strings.HasPrefix(name, "lo") || strings.HasPrefix(name, "gif") || strings.HasPrefix(name, "stf") {
			continue
		}
		in, errIn := strconv.ParseInt(fields[ibytes], 10, 64)
		out, errOut := strconv.ParseInt(fields[obytes], 10, 64)
		if errIn != nil || errOut != nil {
			continue
		}
		rx += in
		tx += out
		counted++
	}
	if counted == 0 {
		return 0, 0, errors.New("collect: netstat listed no physical interfaces")
	}
	return rx, tx, nil
}

// ParseTemperature reads powermetrics' SMC sampler on Intel Macs:
//
//	CPU die temperature: 61.13 C
//
// Apple silicon has no such line, and the caller treats that as "no sensor".
func ParseTemperature(raw []byte) (float32, bool) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "CPU die temperature:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "CPU die temperature:"))
		if len(fields) == 0 {
			return 0, false
		}
		value, err := strconv.ParseFloat(fields[0], 32)
		if err != nil {
			return 0, false
		}
		return float32(value), true
	}
	return 0, false
}

// ParseProcessList reads `ps -Aro pid=,pcpu=,rss=,user=,comm=`: one process
// per line, highest CPU first, with the command path last and possibly
// containing spaces.
func ParseProcessList(raw []byte, limit int) []ProcessSample {
	var out []ProcessSample
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() && len(out) < limit {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		pid, errPID := strconv.ParseInt(fields[0], 10, 32)
		cpu, errCPU := strconv.ParseFloat(fields[1], 32)
		rssKB, errRSS := strconv.ParseInt(fields[2], 10, 64)
		if errPID != nil || errCPU != nil || errRSS != nil {
			continue
		}
		command := strings.Join(fields[4:], " ")
		if i := strings.LastIndex(command, "/"); i >= 0 && i+1 < len(command) {
			command = command[i+1:]
		}
		out = append(out, ProcessSample{
			PID:         int32(pid),
			Name:        command,
			User:        fields[3],
			CPUPercent:  float32(cpu),
			MemoryBytes: rssKB * 1024,
		})
	}
	return out
}

// countLines counts non-empty lines, for `ps -Ao pid=`.
func countLines(raw []byte) int32 {
	var n int32
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			n++
		}
	}
	return n
}
