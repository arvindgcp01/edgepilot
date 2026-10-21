package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/repository/postgres/sqlcgen"
)

// metricsRepo stores and reads device health.
//
// device_metrics carries no org_id and no row-level policy: it is keyed by
// device, and tenancy is enforced one hop up by resolving the device through
// the scoped device repository first. Every method here therefore runs
// unscoped, and every caller is expected to have checked the device.
type metricsRepo struct{ s *Store }

func (r *metricsRepo) InsertSamples(ctx context.Context, samples []domain.MetricSample) error {
	if len(samples) == 0 {
		return nil
	}
	params := make([]sqlcgen.InsertDeviceMetricParams, 0, len(samples))
	for _, m := range samples {
		params = append(params, sqlcgen.InsertDeviceMetricParams{
			DeviceID:         toPgUUID(m.DeviceID),
			Time:             toPgTime(m.Time),
			CpuPercent:       m.CPUPercent,
			CpuUserPercent:   m.CPUUserPercent,
			CpuSystemPercent: m.CPUSystemPercent,
			Load1:            m.Load1,
			Load5:            m.Load5,
			Load15:           m.Load15,
			MemoryUsedBytes:  m.MemoryUsedBytes,
			MemoryTotalBytes: m.MemoryTotalBytes,
			MemoryPressure:   m.MemoryPressure,
			SwapUsedBytes:    m.SwapUsedBytes,
			DiskUsedBytes:    m.DiskUsedBytes,
			DiskTotalBytes:   m.DiskTotalBytes,
			DiskReadBytes:    m.DiskReadBytes,
			DiskWriteBytes:   m.DiskWriteBytes,
			NetRxBytes:       m.NetRxBytes,
			NetTxBytes:       m.NetTxBytes,
			TemperatureC:     m.TemperatureC,
			UptimeSeconds:    m.UptimeSeconds,
			ProcessCount:     m.ProcessCount,
		})
	}

	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		// One pipelined batch: every row travels in a single round trip, and
		// a row that fails — a duplicate (device_id, time) from a retried
		// send, say — fails alone.
		var firstErr error
		results := q.InsertDeviceMetric(ctx, params)
		results.Exec(func(i int, err error) {
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("sample %d: %w", i, err)
			}
		})
		if err := results.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if firstErr != nil {
			return translate(firstErr, "metrics")
		}
		return nil
	})
}

func (r *metricsRepo) UpdateLast(ctx context.Context, last map[uuid.UUID]domain.LastMetrics) error {
	if len(last) == 0 {
		return nil
	}
	p := sqlcgen.UpdateDevicesLastMetricsParams{
		DeviceIds:      make([]pgtype.UUID, 0, len(last)),
		CpuPercents:    make([]float32, 0, len(last)),
		MemoryPercents: make([]float32, 0, len(last)),
		DiskPercents:   make([]float32, 0, len(last)),
		BootedAts:      make([]pgtype.Timestamptz, 0, len(last)),
	}
	for id, m := range last {
		p.DeviceIds = append(p.DeviceIds, toPgUUID(id))
		p.CpuPercents = append(p.CpuPercents, m.CPUPercent)
		// The columns are nullable but the unnest arrays are not, and an
		// unknown percentage is far rarer than a known one. NaN goes through
		// PostgreSQL's real type unchanged and reads back as NULL-equivalent
		// on the way out (see deviceFromRow's callers), so nothing displays 0
		// for a value nobody measured.
		p.MemoryPercents = append(p.MemoryPercents, derefOrNaN(m.MemoryPercent))
		p.DiskPercents = append(p.DiskPercents, derefOrNaN(m.DiskPercent))
		if m.BootedAt != nil {
			p.BootedAts = append(p.BootedAts, toPgTime(*m.BootedAt))
		} else {
			p.BootedAts = append(p.BootedAts, pgtype.Timestamptz{})
		}
	}
	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.UpdateDevicesLastMetrics(ctx, p); err != nil {
			return translate(err, "metrics")
		}
		return nil
	})
}

func (r *metricsRepo) Latest(ctx context.Context, deviceID uuid.UUID) (*domain.MetricSample, error) {
	var out *domain.MetricSample
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.LatestDeviceMetric(ctx, toPgUUID(deviceID))
		if err != nil {
			return translate(err, "metrics")
		}
		out = sampleFromRow(row)
		return nil
	})
	return out, err
}

func (r *metricsRepo) Series(ctx context.Context, in domain.MetricSeriesQuery) ([]domain.MetricBucket, error) {
	if in.Bucket <= 0 {
		return nil, domain.Validation(domain.FieldError{Pointer: "/resolution", Message: "bucket must be positive"})
	}
	var out []domain.MetricBucket
	err := r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.DeviceMetricSeries(ctx, sqlcgen.DeviceMetricSeriesParams{
			BucketSeconds: int64(in.Bucket / time.Second),
			DeviceID:      toPgUUID(in.DeviceID),
			FromTime:      toPgTime(in.From),
			ToTime:        toPgTime(in.To),
		})
		if err != nil {
			return translate(err, "metrics")
		}
		out = make([]domain.MetricBucket, 0, len(rows))
		for _, row := range rows {
			out = append(out, bucketFromRow(row))
		}
		return nil
	})
	return out, err
}

func (r *metricsRepo) EnsurePartitions(ctx context.Context, months []time.Time) error {
	return r.s.unscoped(ctx, func(q *sqlcgen.Queries) error {
		var errs []error
		for _, month := range months {
			first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
			if err := q.EnsureMetricsPartition(ctx, pgtype.Date{Time: first, Valid: true}); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", first.Format("2006-01"), err))
			}
		}
		return errors.Join(errs...)
	})
}

func sampleFromRow(row sqlcgen.DeviceMetric) *domain.MetricSample {
	return &domain.MetricSample{
		DeviceID:         fromPgUUID(row.DeviceID),
		Time:             fromPgTime(row.Time),
		CPUPercent:       row.CpuPercent,
		CPUUserPercent:   row.CpuUserPercent,
		CPUSystemPercent: row.CpuSystemPercent,
		Load1:            row.Load1,
		Load5:            row.Load5,
		Load15:           row.Load15,
		MemoryUsedBytes:  row.MemoryUsedBytes,
		MemoryTotalBytes: row.MemoryTotalBytes,
		MemoryPressure:   row.MemoryPressure,
		SwapUsedBytes:    row.SwapUsedBytes,
		DiskUsedBytes:    row.DiskUsedBytes,
		DiskTotalBytes:   row.DiskTotalBytes,
		DiskReadBytes:    row.DiskReadBytes,
		DiskWriteBytes:   row.DiskWriteBytes,
		NetRxBytes:       row.NetRxBytes,
		NetTxBytes:       row.NetTxBytes,
		TemperatureC:     row.TemperatureC,
		UptimeSeconds:    row.UptimeSeconds,
		ProcessCount:     row.ProcessCount,
	}
}

// bucketFromRow undoes the query's sentinels: NaN and -1 mean "no reading".
func bucketFromRow(row sqlcgen.DeviceMetricSeriesRow) domain.MetricBucket {
	return domain.MetricBucket{
		Bucket:           fromPgTime(row.Bucket),
		CPUPercent:       realOrNil(row.CpuPercent),
		Load1:            realOrNil(row.Load1),
		Load5:            realOrNil(row.Load5),
		Load15:           realOrNil(row.Load15),
		MemoryUsedBytes:  countOrNil(row.MemoryUsedBytes),
		MemoryTotalBytes: countOrNil(row.MemoryTotalBytes),
		MemoryPressure:   realOrNil(row.MemoryPressure),
		DiskUsedBytes:    countOrNil(row.DiskUsedBytes),
		DiskTotalBytes:   countOrNil(row.DiskTotalBytes),
		DiskReadBytes:    countOrNil(row.DiskReadBytes),
		DiskWriteBytes:   countOrNil(row.DiskWriteBytes),
		NetRxBytes:       countOrNil(row.NetRxBytes),
		NetTxBytes:       countOrNil(row.NetTxBytes),
		TemperatureC:     realOrNil(row.TemperatureC),
		UptimeSeconds:    countOrNil(row.UptimeSeconds),
		ProcessCount:     smallOrNil(row.ProcessCount),
	}
}

func realOrNil(v float32) *float32 {
	if math.IsNaN(float64(v)) {
		return nil
	}
	return &v
}

func countOrNil(v int64) *int64 {
	if v < 0 {
		return nil
	}
	return &v
}

func smallOrNil(v int32) *int32 {
	if v < 0 {
		return nil
	}
	return &v
}

func derefOrNaN(v *float32) float32 {
	if v == nil {
		return float32(math.NaN())
	}
	return *v
}
