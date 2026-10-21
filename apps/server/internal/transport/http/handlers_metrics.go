package http

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// metricSampleResponse is the wire shape of one reading — the OpenAPI
// MetricSample, plus the two derived percentages the dashboard shows most.
//
// Nullable fields are pointers without omitempty, so an absent reading is an
// explicit null rather than a missing key. The difference matters to a chart:
// null is a gap, missing is "the server does not know this metric exists".
type metricSampleResponse struct {
	Time             time.Time `json:"time"`
	CPUPercent       float32   `json:"cpu_percent"`
	CPUUserPercent   *float32  `json:"cpu_user_percent"`
	CPUSystemPercent *float32  `json:"cpu_system_percent"`
	Load1            *float32  `json:"load_1"`
	Load5            *float32  `json:"load_5"`
	Load15           *float32  `json:"load_15"`
	MemoryUsedBytes  *int64    `json:"memory_used_bytes"`
	MemoryTotalBytes *int64    `json:"memory_total_bytes"`
	MemoryPercent    *float32  `json:"memory_percent"`
	MemoryPressure   *float32  `json:"memory_pressure"`
	SwapUsedBytes    *int64    `json:"swap_used_bytes"`
	DiskUsedBytes    *int64    `json:"disk_used_bytes"`
	DiskTotalBytes   *int64    `json:"disk_total_bytes"`
	DiskPercent      *float32  `json:"disk_percent"`
	NetRxBytes       *int64    `json:"net_rx_bytes"`
	NetTxBytes       *int64    `json:"net_tx_bytes"`
	TemperatureC     *float32  `json:"temperature_c"`
	UptimeSeconds    *int64    `json:"uptime_seconds"`
	ProcessCount     *int32    `json:"process_count"`
}

func renderSample(m *domain.MetricSample) metricSampleResponse {
	return metricSampleResponse{
		Time:             m.Time,
		CPUPercent:       m.CPUPercent,
		CPUUserPercent:   m.CPUUserPercent,
		CPUSystemPercent: m.CPUSystemPercent,
		Load1:            m.Load1,
		Load5:            m.Load5,
		Load15:           m.Load15,
		MemoryUsedBytes:  m.MemoryUsedBytes,
		MemoryTotalBytes: m.MemoryTotalBytes,
		MemoryPercent:    m.MemoryPercent(),
		MemoryPressure:   m.MemoryPressure,
		SwapUsedBytes:    m.SwapUsedBytes,
		DiskUsedBytes:    m.DiskUsedBytes,
		DiskTotalBytes:   m.DiskTotalBytes,
		DiskPercent:      m.DiskPercent(),
		NetRxBytes:       m.NetRxBytes,
		NetTxBytes:       m.NetTxBytes,
		TemperatureC:     m.TemperatureC,
		UptimeSeconds:    m.UptimeSeconds,
		ProcessCount:     m.ProcessCount,
	}
}

// getLatestMetrics returns the most recent sample for one device.
//
// The device is resolved through the scoped repository first: device_metrics
// has no tenancy column of its own, and this lookup is what stops one
// organization reading another's readings by guessing a UUID.
func (s *Server) getLatestMetrics(w http.ResponseWriter, r *http.Request) error {
	deviceID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if _, err := s.store.Devices().Get(r.Context(), deviceID); err != nil {
		return err
	}
	sample, err := s.store.Metrics().Latest(r.Context(), deviceID)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, renderSample(sample))
}

// metricSeriesResponse is the OpenAPI MetricSeries: named series of
// `[epoch_millis, value|null]` points.
type metricSeriesResponse struct {
	DeviceID   string         `json:"device_id"`
	From       time.Time      `json:"from"`
	To         time.Time      `json:"to"`
	Resolution string         `json:"resolution"`
	Source     string         `json:"source"`
	Series     []metricSeries `json:"series"`
}

type metricSeries struct {
	Name   string   `json:"name"`
	Unit   string   `json:"unit"`
	Points [][2]any `json:"points"`
}

// resolutions maps the OpenAPI resolution names to bucket widths. "raw" is
// the agent's own interval: bucketing at it changes nothing except collapsing
// a duplicated sample.
var resolutions = map[string]time.Duration{
	"raw": 30 * time.Second,
	"1m":  time.Minute,
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"1d":  24 * time.Hour,
}

// autoResolution picks a bucket that keeps a chart to a few hundred points.
func autoResolution(window time.Duration) string {
	switch {
	case window <= 2*time.Hour:
		return "1m"
	case window <= 12*time.Hour:
		return "5m"
	case window <= 48*time.Hour:
		return "15m"
	case window <= 14*24*time.Hour:
		return "1h"
	default:
		return "1d"
	}
}

// maxSeriesWindow bounds one request. Ninety days at a daily bucket is ninety
// rows; ninety days at one minute would be 130,000, which is not a chart.
const maxSeriesWindow = 90 * 24 * time.Hour

// getDeviceMetrics returns bucketed history for one device.
func (s *Server) getDeviceMetrics(w http.ResponseWriter, r *http.Request) error {
	deviceID, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if _, err := s.store.Devices().Get(r.Context(), deviceID); err != nil {
		return err
	}

	query := r.URL.Query()
	from, err := parseTime(query.Get("from"), "/from", time.Time{})
	if err != nil {
		return err
	}
	if from.IsZero() {
		return domain.Validation(domain.FieldError{Pointer: "/from", Message: "is required"})
	}
	to, err := parseTime(query.Get("to"), "/to", time.Now().UTC())
	if err != nil {
		return err
	}
	if !to.After(from) {
		return domain.Validation(domain.FieldError{Pointer: "/to", Message: "must be after from"})
	}
	if to.Sub(from) > maxSeriesWindow {
		return domain.Validation(domain.FieldError{Pointer: "/from", Message: "the window may not exceed 90 days"})
	}

	resolution := strings.TrimSpace(query.Get("resolution"))
	if resolution == "" || resolution == "auto" {
		resolution = autoResolution(to.Sub(from))
	}
	bucket, ok := resolutions[resolution]
	if !ok {
		return domain.Validation(domain.FieldError{
			Pointer: "/resolution", Message: "must be one of auto, raw, 1m, 5m, 15m, 1h, 1d",
		})
	}
	// A fine resolution over a wide window is the 130,000-row chart again,
	// just spelled explicitly. Coarsen rather than refuse.
	for to.Sub(from)/bucket > 2000 {
		resolution = coarser(resolution)
		bucket = resolutions[resolution]
	}

	wanted := selectedMetrics(query.Get("metrics"))

	buckets, err := s.store.Metrics().Series(r.Context(), domain.MetricSeriesQuery{
		DeviceID: deviceID, From: from, To: to, Bucket: bucket,
	})
	if err != nil {
		return err
	}

	return respond(w, http.StatusOK, metricSeriesResponse{
		DeviceID:   deviceID.String(),
		From:       from,
		To:         to,
		Resolution: resolution,
		Source:     "raw",
		Series:     buildSeries(buckets, bucket, wanted),
	})
}

func coarser(resolution string) string {
	order := []string{"raw", "1m", "5m", "15m", "1h", "1d"}
	for i, r := range order {
		if r == resolution && i+1 < len(order) {
			return order[i+1]
		}
	}
	return "1d"
}

func parseTime(raw, pointer string, fallback time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, domain.Validation(domain.FieldError{Pointer: pointer, Message: "must be an RFC 3339 timestamp"})
	}
	return t.UTC(), nil
}

// metricGroups maps the OpenAPI `metrics` filter values to series names.
var metricGroups = map[string][]string{
	"cpu":             {"cpu_percent"},
	"memory":          {"memory_percent", "memory_used_bytes", "memory_total_bytes"},
	"memory_pressure": {"memory_pressure"},
	"disk":            {"disk_percent", "disk_used_bytes", "disk_total_bytes"},
	"load":            {"load_1", "load_5", "load_15"},
	"network":         {"net_rx_bytes_per_sec", "net_tx_bytes_per_sec"},
	"disk_io":         {"disk_read_bytes_per_sec", "disk_write_bytes_per_sec"},
	"temperature":     {"temperature_c"},
	"uptime":          {"uptime_seconds", "process_count"},
}

// selectedMetrics resolves the filter to a set of series names. Empty means
// everything, and an unknown name is ignored rather than rejected: a newer
// dashboard asking an older server for a metric it does not have should get
// the ones it does have.
func selectedMetrics(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, group := range strings.Split(raw, ",") {
		for _, name := range metricGroups[strings.TrimSpace(group)] {
			out[name] = true
		}
	}
	return out
}

// buildSeries turns buckets into named point lists.
//
// A gap wider than two buckets becomes an explicit null point, so a chart
// draws a break where the device was silent instead of a confident line
// across an outage. Counters are differenced into per-second rates between
// consecutive buckets; the first bucket has no rate, and a counter reset —
// the Mac rebooted — reads as a negative delta and yields null.
func buildSeries(buckets []domain.MetricBucket, width time.Duration, wanted map[string]bool) []metricSeries {
	type def struct {
		name, unit string
		value      func(i int) *float64
	}

	gauge32 := func(get func(domain.MetricBucket) *float32) func(int) *float64 {
		return func(i int) *float64 {
			v := get(buckets[i])
			if v == nil || math.IsNaN(float64(*v)) {
				return nil
			}
			f := float64(*v)
			return &f
		}
	}
	gauge64 := func(get func(domain.MetricBucket) *int64) func(int) *float64 {
		return func(i int) *float64 {
			v := get(buckets[i])
			if v == nil {
				return nil
			}
			f := float64(*v)
			return &f
		}
	}
	percent := func(used, total func(domain.MetricBucket) *int64) func(int) *float64 {
		return func(i int) *float64 {
			u, t := used(buckets[i]), total(buckets[i])
			if u == nil || t == nil || *t <= 0 {
				return nil
			}
			f := math.Min(100, math.Max(0, float64(*u)/float64(*t)*100))
			return &f
		}
	}
	rate := func(get func(domain.MetricBucket) *int64) func(int) *float64 {
		return func(i int) *float64 {
			if i == 0 {
				return nil
			}
			prev, cur := get(buckets[i-1]), get(buckets[i])
			if prev == nil || cur == nil || *cur < *prev {
				return nil
			}
			seconds := buckets[i].Bucket.Sub(buckets[i-1].Bucket).Seconds()
			if seconds <= 0 {
				return nil
			}
			f := float64(*cur-*prev) / seconds
			return &f
		}
	}

	defs := []def{
		{"cpu_percent", "percent", gauge32(func(b domain.MetricBucket) *float32 { return b.CPUPercent })},
		{"memory_percent", "percent", percent(
			func(b domain.MetricBucket) *int64 { return b.MemoryUsedBytes },
			func(b domain.MetricBucket) *int64 { return b.MemoryTotalBytes })},
		{"memory_used_bytes", "bytes", gauge64(func(b domain.MetricBucket) *int64 { return b.MemoryUsedBytes })},
		{"memory_total_bytes", "bytes", gauge64(func(b domain.MetricBucket) *int64 { return b.MemoryTotalBytes })},
		{"memory_pressure", "percent", gauge32(func(b domain.MetricBucket) *float32 { return b.MemoryPressure })},
		{"disk_percent", "percent", percent(
			func(b domain.MetricBucket) *int64 { return b.DiskUsedBytes },
			func(b domain.MetricBucket) *int64 { return b.DiskTotalBytes })},
		{"disk_used_bytes", "bytes", gauge64(func(b domain.MetricBucket) *int64 { return b.DiskUsedBytes })},
		{"disk_total_bytes", "bytes", gauge64(func(b domain.MetricBucket) *int64 { return b.DiskTotalBytes })},
		{"load_1", "load", gauge32(func(b domain.MetricBucket) *float32 { return b.Load1 })},
		{"load_5", "load", gauge32(func(b domain.MetricBucket) *float32 { return b.Load5 })},
		{"load_15", "load", gauge32(func(b domain.MetricBucket) *float32 { return b.Load15 })},
		{"net_rx_bytes_per_sec", "bytes_per_second", rate(func(b domain.MetricBucket) *int64 { return b.NetRxBytes })},
		{"net_tx_bytes_per_sec", "bytes_per_second", rate(func(b domain.MetricBucket) *int64 { return b.NetTxBytes })},
		{"disk_read_bytes_per_sec", "bytes_per_second", rate(func(b domain.MetricBucket) *int64 { return b.DiskReadBytes })},
		{"disk_write_bytes_per_sec", "bytes_per_second", rate(func(b domain.MetricBucket) *int64 { return b.DiskWriteBytes })},
		{"temperature_c", "celsius", gauge32(func(b domain.MetricBucket) *float32 { return b.TemperatureC })},
		{"uptime_seconds", "seconds", gauge64(func(b domain.MetricBucket) *int64 { return b.UptimeSeconds })},
		{"process_count", "count", func(i int) *float64 {
			v := buckets[i].ProcessCount
			if v == nil {
				return nil
			}
			f := float64(*v)
			return &f
		}},
	}

	out := make([]metricSeries, 0, len(defs))
	for _, d := range defs {
		if wanted != nil && !wanted[d.name] {
			continue
		}
		points := make([][2]any, 0, len(buckets)+len(buckets)/8)
		for i, b := range buckets {
			if i > 0 && b.Bucket.Sub(buckets[i-1].Bucket) > 2*width {
				gap := buckets[i-1].Bucket.Add(width)
				points = append(points, [2]any{gap.UnixMilli(), nil})
			}
			if v := d.value(i); v != nil {
				points = append(points, [2]any{b.Bucket.UnixMilli(), *v})
			} else {
				points = append(points, [2]any{b.Bucket.UnixMilli(), nil})
			}
		}
		out = append(out, metricSeries{Name: d.name, Unit: d.unit, Points: points})
	}
	return out
}
