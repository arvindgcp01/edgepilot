DROP TABLE IF EXISTS device_events CASCADE;
DROP TABLE IF EXISTS device_metrics_hourly CASCADE;
DROP TABLE IF EXISTS device_metrics CASCADE;
DROP FUNCTION IF EXISTS ensure_monthly_partition(regclass, date);
