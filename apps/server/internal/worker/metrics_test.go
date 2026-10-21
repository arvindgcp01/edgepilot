package worker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/worker"
)

// A recording repository, so the flusher's batching behaviour is tested
// without a database. The repository itself is covered against Postgres in
// the repository package; what matters here is what reaches it and when.
type recordingMetrics struct {
	mu       sync.Mutex
	inserted [][]domain.MetricSample
	last     []map[uuid.UUID]domain.LastMetrics
	months   []time.Time
	fail     error
}

func (r *recordingMetrics) InsertSamples(_ context.Context, samples []domain.MetricSample) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.inserted = append(r.inserted, samples)
	return nil
}

func (r *recordingMetrics) UpdateLast(_ context.Context, last map[uuid.UUID]domain.LastMetrics) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.last = append(r.last, last)
	return nil
}

func (r *recordingMetrics) Latest(context.Context, uuid.UUID) (*domain.MetricSample, error) {
	return nil, errors.New("not used")
}

func (r *recordingMetrics) Series(context.Context, domain.MetricSeriesQuery) ([]domain.MetricBucket, error) {
	return nil, errors.New("not used")
}

func (r *recordingMetrics) EnsurePartitions(_ context.Context, months []time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.months = append(r.months, months...)
	return nil
}

func sampleAt(device uuid.UUID, at time.Time, cpu float32) domain.MetricSample {
	used, total, up := int64(8<<30), int64(16<<30), int64(3600)
	return domain.MetricSample{
		DeviceID: device, Time: at, CPUPercent: cpu,
		MemoryUsedBytes: &used, MemoryTotalBytes: &total, UptimeSeconds: &up,
	}
}

func TestFlushWritesEverySampleButOnlyTheLatestPerDevice(t *testing.T) {
	repo := &recordingMetrics{}
	f := worker.NewMetricsFlusher(worker.MetricsFlusherOptions{Metrics: repo, Logger: quiet()})
	device := uuid.New()
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	f.Record(sampleAt(device, base, 10))
	f.Record(sampleAt(device, base.Add(30*time.Second), 20))
	f.Record(sampleAt(device, base.Add(60*time.Second), 30))
	f.Flush(t.Context())

	if len(repo.inserted) != 1 || len(repo.inserted[0]) != 3 {
		t.Fatalf("inserted batches = %v, want one batch of three samples", repo.inserted)
	}
	if len(repo.last) != 1 || len(repo.last[0]) != 1 {
		t.Fatalf("last-metrics writes = %v, want one write for one device", repo.last)
	}
	last := repo.last[0][device]
	if last.CPUPercent != 30 {
		t.Errorf("last cpu = %v, want the newest sample's 30", last.CPUPercent)
	}
	if last.MemoryPercent == nil || *last.MemoryPercent != 50 {
		t.Errorf("last memory percent = %v, want 50", last.MemoryPercent)
	}
	if last.BootedAt == nil || !last.BootedAt.Equal(base.Add(60*time.Second).Add(-time.Hour)) {
		t.Errorf("booted_at = %v, want one hour before the newest sample", last.BootedAt)
	}
}

func TestRecordNeverLetsAnOlderSampleBecomeLatest(t *testing.T) {
	repo := &recordingMetrics{}
	f := worker.NewMetricsFlusher(worker.MetricsFlusherOptions{Metrics: repo, Logger: quiet()})
	device := uuid.New()
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	f.Record(sampleAt(device, base.Add(time.Minute), 80))
	f.Record(sampleAt(device, base, 5)) // late arrival from an overlapping stream
	f.Flush(t.Context())

	if got := repo.last[0][device].CPUPercent; got != 80 {
		t.Errorf("last cpu = %v, want 80 from the newer sample", got)
	}
	if len(repo.inserted[0]) != 2 {
		t.Errorf("both raw samples must still be stored, got %d", len(repo.inserted[0]))
	}
}

func TestFlushingIsFreeWhenNothingIsRecorded(t *testing.T) {
	repo := &recordingMetrics{}
	f := worker.NewMetricsFlusher(worker.MetricsFlusherOptions{Metrics: repo, Logger: quiet()})
	f.Flush(t.Context())
	if len(repo.inserted) != 0 || len(repo.last) != 0 {
		t.Error("an empty flush must not touch the repository")
	}
}

func TestAFailedFlushDropsTheBatchRatherThanGrowing(t *testing.T) {
	repo := &recordingMetrics{fail: errors.New("database away")}
	f := worker.NewMetricsFlusher(worker.MetricsFlusherOptions{Metrics: repo, Logger: quiet()})
	f.Record(sampleAt(uuid.New(), time.Now(), 1))
	f.Flush(t.Context())
	if f.Pending() != 0 {
		t.Errorf("pending = %d after a failed flush, want 0", f.Pending())
	}
	if f.Failures() != 2 {
		t.Errorf("failures = %d, want 2 (samples and last-metrics)", f.Failures())
	}
}

func TestTheBufferIsBounded(t *testing.T) {
	repo := &recordingMetrics{}
	f := worker.NewMetricsFlusher(worker.MetricsFlusherOptions{Metrics: repo, Logger: quiet(), MaxPending: 10})
	device := uuid.New()
	for i := range 25 {
		f.Record(sampleAt(device, time.Now().Add(time.Duration(i)*time.Second), float32(i)))
	}
	if f.Pending() != 10 {
		t.Errorf("pending = %d, want the cap of 10", f.Pending())
	}
	f.Flush(t.Context())
	// The newest samples survive; the oldest were the ones dropped.
	kept := repo.inserted[0]
	if kept[0].CPUPercent != 15 || kept[len(kept)-1].CPUPercent != 24 {
		t.Errorf("kept %v..%v, want the newest ten (15..24)", kept[0].CPUPercent, kept[len(kept)-1].CPUPercent)
	}
}

func TestRunEnsuresThisMonthAndNextOnStart(t *testing.T) {
	repo := &recordingMetrics{}
	now := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	f := worker.NewMetricsFlusher(worker.MetricsFlusherOptions{
		Metrics: repo, Logger: quiet(), Interval: time.Hour, Now: func() time.Time { return now },
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for {
		repo.mu.Lock()
		n := len(repo.months)
		repo.mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("partitions were not ensured on start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if !repo.months[0].Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) ||
		!repo.months[1].Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ensured %v, want December 2026 and January 2027", repo.months)
	}
}
