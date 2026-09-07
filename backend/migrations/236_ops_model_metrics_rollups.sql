-- Model-scoped Ops rollups. These tables intentionally contain only rows with
-- a model dimension; the existing model-less hourly/daily tables remain small.
CREATE TABLE IF NOT EXISTS ops_model_metrics_5m (
    id BIGSERIAL PRIMARY KEY,
    bucket_start TIMESTAMPTZ NOT NULL,
    model VARCHAR(255) NOT NULL,
    platform VARCHAR(32),
    group_id BIGINT,
    success_count BIGINT NOT NULL DEFAULT 0,
    ttft_sample_count BIGINT NOT NULL DEFAULT 0,
    error_count_total BIGINT NOT NULL DEFAULT 0,
    business_limited_count BIGINT NOT NULL DEFAULT 0,
    error_count_sla BIGINT NOT NULL DEFAULT 0,
    upstream_error_count_excl_429_529 BIGINT NOT NULL DEFAULT 0,
    upstream_429_count BIGINT NOT NULL DEFAULT 0,
    upstream_529_count BIGINT NOT NULL DEFAULT 0,
    token_consumed BIGINT NOT NULL DEFAULT 0,
    switch_count BIGINT NOT NULL DEFAULT 0,
    duration_p50_ms INT, duration_p90_ms INT, duration_p95_ms INT, duration_p99_ms INT,
    duration_avg_ms DOUBLE PRECISION, duration_max_ms INT,
    ttft_p50_ms INT, ttft_p90_ms INT, ttft_p95_ms INT, ttft_p99_ms INT,
    ttft_avg_ms DOUBLE PRECISION, ttft_max_ms INT,
    latency_0_100 BIGINT NOT NULL DEFAULT 0,
    latency_100_200 BIGINT NOT NULL DEFAULT 0,
    latency_200_500 BIGINT NOT NULL DEFAULT 0,
    latency_500_1000 BIGINT NOT NULL DEFAULT 0,
    latency_1000_2000 BIGINT NOT NULL DEFAULT 0,
    latency_2000_plus BIGINT NOT NULL DEFAULT 0,
    error_status_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ops_model_metrics_5m_model_nonempty CHECK (btrim(model) <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ops_model_metrics_5m_unique_dim
    ON ops_model_metrics_5m (bucket_start, model, COALESCE(platform, ''), COALESCE(group_id, 0));
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_5m_model_bucket
    ON ops_model_metrics_5m (model, bucket_start DESC);
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_5m_model_platform_bucket
    ON ops_model_metrics_5m (model, platform, bucket_start DESC) WHERE group_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_5m_model_group_bucket
    ON ops_model_metrics_5m (model, group_id, bucket_start DESC) WHERE group_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS ops_model_metrics_hourly (LIKE ops_model_metrics_5m INCLUDING DEFAULTS INCLUDING CONSTRAINTS);
CREATE SEQUENCE IF NOT EXISTS ops_model_metrics_hourly_id_seq OWNED BY ops_model_metrics_hourly.id;
ALTER TABLE ops_model_metrics_hourly ALTER COLUMN id SET DEFAULT nextval('ops_model_metrics_hourly_id_seq');
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'ops_model_metrics_hourly'::regclass AND contype = 'p'
    ) THEN
        ALTER TABLE ops_model_metrics_hourly ADD PRIMARY KEY (id);
    END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS idx_ops_model_metrics_hourly_unique_dim
    ON ops_model_metrics_hourly (bucket_start, model, COALESCE(platform, ''), COALESCE(group_id, 0));
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_hourly_model_bucket
    ON ops_model_metrics_hourly (model, bucket_start DESC);
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_hourly_model_platform_bucket
    ON ops_model_metrics_hourly (model, platform, bucket_start DESC) WHERE group_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_hourly_model_group_bucket
    ON ops_model_metrics_hourly (model, group_id, bucket_start DESC) WHERE group_id IS NOT NULL;

-- A completed bucket is recorded even when it contains no rows. Query routing
-- uses this table to distinguish a legitimate zero-traffic bucket from a hole.
CREATE TABLE IF NOT EXISTS ops_model_metrics_coverage (
    resolution_seconds INT NOT NULL CHECK (resolution_seconds IN (300, 3600)),
    bucket_start TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (resolution_seconds, bucket_start)
);
CREATE INDEX IF NOT EXISTS idx_ops_model_metrics_coverage_completed
    ON ops_model_metrics_coverage (completed_at DESC);

COMMENT ON TABLE ops_model_metrics_5m IS 'Model-scoped five-minute Ops rollups retained for 14 days.';
COMMENT ON TABLE ops_model_metrics_hourly IS 'Model-scoped hourly Ops rollups retained for 90 days.';
COMMENT ON TABLE ops_model_metrics_coverage IS 'Completion markers for model rollup buckets, including empty buckets.';
