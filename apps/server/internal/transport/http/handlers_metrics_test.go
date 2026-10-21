package http

import (
	"testing"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

func f32(v float32) *float32 { return &v }
func i64(v int64) *int64     { return &v }

func TestBuildSeriesDerivesPercentagesAndRates(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	buckets := []domain.MetricBucket{
		{Bucket: base, CPUPercent: f32(10), MemoryUsedBytes: i64(4 << 30), MemoryTotalBytes: i64(16 << 30), NetRxBytes: i64(1000)},
		{Bucket: base.Add(time.Minute), CPUPercent: f32(20), MemoryUsedBytes: i64(8 << 30), MemoryTotalBytes: i64(16 << 30), NetRxBytes: i64(7000)},
	}
	series := buildSeries(buckets, time.Minute, map[string]bool{"cpu_percent": true, "memory_percent": true, "net_rx_bytes_per_sec": true})

	byName := map[string]metricSeries{}
	for _, s := range series {
		byName[s.Name] = s
	}
	if len(byName) != 3 {
		t.Fatalf("got series %v, want exactly the three requested", keys(byName))
	}

	if got := byName["memory_percent"].Points[1][1]; got != 50.0 {
		t.Errorf("memory_percent = %v, want 50", got)
	}
	// 6000 bytes over 60 seconds.
	if got := byName["net_rx_bytes_per_sec"].Points[1][1]; got != 100.0 {
		t.Errorf("net_rx rate = %v, want 100", got)
	}
	if got := byName["net_rx_bytes_per_sec"].Points[0][1]; got != nil {
		t.Errorf("the first bucket has no rate, got %v", got)
	}
}

func TestBuildSeriesMarksAGapWithANullPoint(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	buckets := []domain.MetricBucket{
		{Bucket: base, CPUPercent: f32(10)},
		// Ten minutes later at a one-minute bucket: the device was silent.
		{Bucket: base.Add(10 * time.Minute), CPUPercent: f32(12)},
	}
	series := buildSeries(buckets, time.Minute, map[string]bool{"cpu_percent": true})
	points := series[0].Points
	if len(points) != 3 {
		t.Fatalf("got %d points, want 3 (value, gap, value)", len(points))
	}
	if points[1][1] != nil {
		t.Errorf("the gap point must be null, got %v", points[1][1])
	}
	if points[1][0] != base.Add(time.Minute).UnixMilli() {
		t.Errorf("the gap sits one bucket after the last reading, got %v", points[1][0])
	}
}

func TestBuildSeriesTreatsACounterResetAsUnknown(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	buckets := []domain.MetricBucket{
		{Bucket: base, NetTxBytes: i64(9_000_000)},
		{Bucket: base.Add(time.Minute), NetTxBytes: i64(500)}, // rebooted
	}
	series := buildSeries(buckets, time.Minute, map[string]bool{"net_tx_bytes_per_sec": true})
	if got := series[0].Points[1][1]; got != nil {
		t.Errorf("a negative delta must not become a rate, got %v", got)
	}
}

func TestBuildSeriesWithNoFilterReturnsEverything(t *testing.T) {
	series := buildSeries(nil, time.Minute, nil)
	if len(series) != 18 {
		t.Errorf("got %d series, want all 18", len(series))
	}
}

func TestAutoResolutionKeepsChartsToAFewHundredPoints(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:           "1m",
		6 * time.Hour:       "5m",
		24 * time.Hour:      "15m",
		7 * 24 * time.Hour:  "1h",
		60 * 24 * time.Hour: "1d",
	}
	for window, want := range cases {
		if got := autoResolution(window); got != want {
			t.Errorf("autoResolution(%s) = %s, want %s", window, got, want)
		}
		if points := window / resolutions[want]; points > 2000 {
			t.Errorf("%s at %s is %d points", window, want, points)
		}
	}
}

func TestSelectedMetricsIgnoresUnknownNames(t *testing.T) {
	got := selectedMetrics("cpu, bogus ,load")
	for _, name := range []string{"cpu_percent", "load_1", "load_5", "load_15"} {
		if !got[name] {
			t.Errorf("%s missing from %v", name, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %v, want exactly cpu and load series", got)
	}
	if selectedMetrics("") != nil {
		t.Error("an empty filter means everything and must be nil")
	}
}

func keys(m map[string]metricSeries) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
