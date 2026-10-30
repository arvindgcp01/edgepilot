package collect

import (
	"bufio"
	"errors"
	"strconv"
	"strings"
)

// Parsers for the procfs files the Linux metrics collector reads. Kept free of
// build tags, like the rest of the parsing, so they are tested on every
// platform against captured file contents.

// CPUTimes is the aggregate "cpu" line of /proc/stat, in clock ticks.
type CPUTimes struct {
	User, System, Idle, Total uint64
}

// ParseProcStatCPU reads the aggregate cpu line of /proc/stat.
//
// The columns are user nice system idle iowait irq softirq steal guest
// guest_nice. Guest time is already counted inside user and nice, so it is left
// out of the total; iowait counts as idle, which is what top shows as "id".
func ParseProcStatCPU(raw []byte) (CPUTimes, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var v [8]uint64
		for i := 0; i < 8 && i+1 < len(fields); i++ {
			n, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return CPUTimes{}, errors.New("collect: /proc/stat cpu line is malformed")
			}
			v[i] = n
		}
		user, nice, system, idle, iowait, irq, softirq, steal := v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]
		return CPUTimes{
			User:   user + nice,
			System: system + irq + softirq,
			Idle:   idle + iowait,
			Total:  user + nice + system + idle + iowait + irq + softirq + steal,
		}, nil
	}
	return CPUTimes{}, errors.New("collect: /proc/stat has no cpu line")
}

// CPUPercents turns two readings into user, system and busy percentages.
func CPUPercents(before, after CPUTimes) (user, system, busy float32, ok bool) {
	if after.Total <= before.Total {
		return 0, 0, 0, false
	}
	total := float64(after.Total - before.Total)
	pct := func(a, b uint64) float32 {
		if a < b {
			return 0
		}
		return float32(float64(a-b) / total * 100)
	}
	user = pct(after.User, before.User)
	system = pct(after.System, before.System)
	busy = 100 - pct(after.Idle, before.Idle)
	return clamp(user, 0, 100), clamp(system, 0, 100), clamp(busy, 0, 100), true
}

// MemInfo is the part of /proc/meminfo the sample needs, in bytes.
type MemInfo struct {
	Total, Available, SwapTotal, SwapFree int64
}

// ParseMemInfo reads /proc/meminfo.
//
// "Used" is Total minus MemAvailable: page cache the kernel would hand back on
// demand is not memory in use, the same distinction memory_pressure draws on a
// Mac. Kernels before 3.14 have no MemAvailable; free + buffers + cached is the
// classic approximation there.
func ParseMemInfo(raw []byte) (MemInfo, error) {
	values := map[string]int64{}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		key, rest, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		values[key] = n
	}
	info := MemInfo{
		Total:     values["MemTotal"],
		Available: values["MemAvailable"],
		SwapTotal: values["SwapTotal"],
		SwapFree:  values["SwapFree"],
	}
	if info.Total == 0 {
		return MemInfo{}, errors.New("collect: /proc/meminfo has no MemTotal")
	}
	if _, has := values["MemAvailable"]; !has {
		info.Available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if info.Available > info.Total {
		info.Available = info.Total
	}
	return info, nil
}

// Used is memory in use, excluding reclaimable cache.
func (m MemInfo) Used() int64 { return m.Total - m.Available }

// ParseProcLoadAvg reads /proc/loadavg: "0.52 0.58 0.59 1/467 12345".
func ParseProcLoadAvg(raw []byte) (l1, l5, l15 float32, err error) {
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return 0, 0, 0, errors.New("collect: /proc/loadavg is malformed")
	}
	var v [3]float32
	for i := range v {
		f, err := strconv.ParseFloat(fields[i], 32)
		if err != nil {
			return 0, 0, 0, errors.New("collect: /proc/loadavg is malformed")
		}
		v[i] = float32(f)
	}
	return v[0], v[1], v[2], nil
}

// ParseProcNetDev sums received and transmitted bytes from /proc/net/dev.
//
// Loopback and the virtual interfaces container runtimes create (docker0,
// veth*, br-*, cni*, flannel*) are skipped: traffic between containers on the
// same box would otherwise be counted twice and dwarf the real uplink, which is
// the number an operator is asking about.
func ParseProcNetDev(raw []byte) (rx, tx int64, err error) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	seen := false
	for scanner.Scan() {
		name, rest, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if skipInterface(name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, errR := strconv.ParseInt(fields[0], 10, 64)
		t, errT := strconv.ParseInt(fields[8], 10, 64)
		if errR != nil || errT != nil {
			continue
		}
		rx += r
		tx += t
		seen = true
	}
	if !seen {
		return 0, 0, errors.New("collect: /proc/net/dev lists no interfaces")
	}
	return rx, tx, nil
}

func skipInterface(name string) bool {
	if name == "lo" || name == "docker0" {
		return true
	}
	for _, prefix := range []string{"veth", "br-", "cni", "flannel", "virbr", "tailscale", "zt"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// PickTemperature chooses the CPU temperature from thermal zones.
//
// zones maps a zone's type to its reading in millidegrees. A CPU package sensor
// wins when there is one (x86_pkg_temp on Intel, cpu-thermal / cpu_thermal on
// ARM boards, k10temp's Tctl surfaces through hwmon instead and is not here).
// Otherwise the hottest plausible zone is the honest answer: an operator asking
// "is this box cooking" wants the worst reading, not an average. Readings
// outside 0–150 °C are a sensor reporting garbage, not a machine on fire.
func PickTemperature(zones map[string]int64) (float32, bool) {
	for _, preferred := range []string{"x86_pkg_temp", "cpu-thermal", "cpu_thermal", "soc_thermal", "cpu"} {
		if milli, ok := zones[preferred]; ok && milli > 0 && milli < 150000 {
			return float32(milli) / 1000, true
		}
	}
	var best int64 = -1
	for _, milli := range zones {
		if milli > 0 && milli < 150000 && milli > best {
			best = milli
		}
	}
	if best < 0 {
		return 0, false
	}
	return float32(best) / 1000, true
}

// clamp bounds a percentage; NaN, which a zero-length interval can produce,
// becomes the lower bound.
func clamp(v, lo, hi float32) float32 {
	if v != v { // NaN
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
