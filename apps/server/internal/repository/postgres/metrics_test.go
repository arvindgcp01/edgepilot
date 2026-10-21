package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

func metricSample(device uuid.UUID, at time.Time, cpu float32, memUsed int64, temp *float32) domain.MetricSample {
	total := int64(16 << 30)
	diskUsed, diskTotal := int64(200<<30), int64(500<<30)
	up := int64(7200)
	rx := int64(1_000_000)
	return domain.MetricSample{
		DeviceID: device, Time: at, CPUPercent: cpu,
		MemoryUsedBytes: &memUsed, MemoryTotalBytes: &total,
		DiskUsedBytes: &diskUsed, DiskTotalBytes: &diskTotal,
		UptimeSeconds: &up, NetRxBytes: &rx, TemperatureC: temp,
	}
}

func TestMetricsRoundTrip(t *testing.T) {
	f := newFixture(t)
	org := f.createOrg(t, "Metrics Co", "metrics-"+randomSuffix(t))
	ctx := f.asOrg(t, org)
	device := f.enrollDevice(t, org, "SERIAL-METRICS-1", "metrics-mac")
	repo := f.store.Metrics()

	base := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	temp := float32(52.5)
	samples := []domain.MetricSample{
		metricSample(device.ID, base, 10, 4<<30, nil),
		metricSample(device.ID, base.Add(30*time.Second), 20, 6<<30, nil),
		metricSample(device.ID, base.Add(60*time.Second), 30, 8<<30, &temp),
		// A device that does not exist: dropped, not fatal.
		metricSample(uuid.New(), base, 99, 1<<30, nil),
	}
	if err := repo.InsertSamples(ctx, samples); err != nil {
		t.Fatalf("InsertSamples: %v", err)
	}

	latest, err := repo.Latest(ctx, device.ID)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.CPUPercent != 30 || latest.TemperatureC == nil || *latest.TemperatureC != 52.5 {
		t.Errorf("latest = cpu %v temp %v, want 30 and 52.5", latest.CPUPercent, latest.TemperatureC)
	}
	if got := latest.MemoryPercent(); got == nil || *got != 50 {
		t.Errorf("latest memory percent = %v, want 50", got)
	}

	// One-minute buckets: the first minute averages two samples, the second
	// holds one; temperature is unknown in the first and 52.5 in the second.
	buckets, err := repo.Series(ctx, domain.MetricSeriesQuery{
		DeviceID: device.ID, From: base.Add(-time.Minute), To: base.Add(5 * time.Minute), Bucket: time.Minute,
	})
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(buckets), buckets)
	}
	if buckets[0].CPUPercent == nil || *buckets[0].CPUPercent != 15 {
		t.Errorf("first bucket cpu = %v, want the average 15", buckets[0].CPUPercent)
	}
	if buckets[0].TemperatureC != nil {
		t.Errorf("first bucket temperature = %v, want nil (no reading, not zero)", *buckets[0].TemperatureC)
	}
	if buckets[1].TemperatureC == nil || *buckets[1].TemperatureC != 52.5 {
		t.Errorf("second bucket temperature = %v, want 52.5", buckets[1].TemperatureC)
	}
	if buckets[1].NetRxBytes == nil || *buckets[1].NetRxBytes != 1_000_000 {
		t.Errorf("counter carried as max = %v", buckets[1].NetRxBytes)
	}
	if buckets[0].DiskReadBytes != nil {
		t.Errorf("a counter nobody reported must be nil, got %v", *buckets[0].DiskReadBytes)
	}

	// The denormalised columns reach the device row.
	mem := float32(50)
	booted := base.Add(60 * time.Second).Add(-2 * time.Hour)
	if err := repo.UpdateLast(ctx, map[uuid.UUID]domain.LastMetrics{
		device.ID: {CPUPercent: 30, MemoryPercent: &mem, DiskPercent: nil, BootedAt: &booted, At: base.Add(60 * time.Second)},
	}); err != nil {
		t.Fatalf("UpdateLast: %v", err)
	}
	updated, err := f.store.Devices().Get(ctx, device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastCPUPercent == nil || *updated.LastCPUPercent != 30 {
		t.Errorf("last_cpu_percent = %v, want 30", updated.LastCPUPercent)
	}
	if updated.LastMemoryPercent == nil || *updated.LastMemoryPercent != 50 {
		t.Errorf("last_memory_percent = %v, want 50", updated.LastMemoryPercent)
	}
	if updated.LastBootAt == nil || !updated.LastBootAt.Equal(booted) {
		t.Errorf("last_boot_at = %v, want %v", updated.LastBootAt, booted)
	}
}

func TestLatestIsNotFoundForADeviceThatNeverReported(t *testing.T) {
	f := newFixture(t)
	org := f.createOrg(t, "Quiet Co", "quiet-"+randomSuffix(t))
	ctx := f.asOrg(t, org)
	device := f.enrollDevice(t, org, "SERIAL-QUIET-1", "quiet-mac")

	_, err := f.store.Metrics().Latest(ctx, device.ID)
	var notFound *domain.Error
	if err == nil || !errors.As(err, &notFound) || notFound.Kind != domain.KindNotFound {
		t.Errorf("got %v, want a NotFound domain error", err)
	}
}

func TestEnsurePartitionsIsIdempotent(t *testing.T) {
	f := newFixture(t)
	org := f.createOrg(t, "Partition Co", "partition-"+randomSuffix(t))
	ctx := f.asOrg(t, org)
	month := time.Date(2031, 3, 15, 0, 0, 0, 0, time.UTC)
	for range 2 {
		if err := f.store.Metrics().EnsurePartitions(ctx, []time.Time{month}); err != nil {
			t.Fatalf("EnsurePartitions: %v", err)
		}
	}
	// And a sample lands in it.
	device := f.enrollDevice(t, org, "SERIAL-PART-1", "part-mac")
	if err := f.store.Metrics().InsertSamples(ctx, []domain.MetricSample{metricSample(device.ID, month, 1, 1<<30, nil)}); err != nil {
		t.Fatalf("insert into a freshly created partition: %v", err)
	}
}
