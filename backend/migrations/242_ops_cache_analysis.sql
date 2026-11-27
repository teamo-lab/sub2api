-- 缓存命中根因分析的 5 分钟聚合。每个网关进程把内存聚合定期“累加写入”，
-- 同一维度多次写入在 ON CONFLICT 里相加。cause 取值见 service.CacheCause。
-- tokens 是归到该原因的 prompt token：命中请求的 cache_read 记 hit、未缓存部分记 incremental，
-- 未命中请求的全部 prompt token 记到对应原因。
CREATE TABLE IF NOT EXISTS ops_cache_analysis_5m (
    bucket_start  TIMESTAMPTZ NOT NULL,
    user_id       BIGINT NOT NULL DEFAULT 0,
    group_id      BIGINT NOT NULL DEFAULT 0,
    account_id    BIGINT NOT NULL DEFAULT 0,
    model         VARCHAR(255) NOT NULL DEFAULT '',
    protocol      VARCHAR(32) NOT NULL DEFAULT '',
    cause         VARCHAR(32) NOT NULL,
    request_count BIGINT NOT NULL DEFAULT 0,
    tokens        BIGINT NOT NULL DEFAULT 0,
    saved_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (bucket_start, user_id, group_id, account_id, model, protocol, cause)
);

CREATE INDEX IF NOT EXISTS idx_ops_cache_analysis_5m_account_bucket
    ON ops_cache_analysis_5m (account_id, bucket_start DESC);
CREATE INDEX IF NOT EXISTS idx_ops_cache_analysis_5m_group_bucket
    ON ops_cache_analysis_5m (group_id, bucket_start DESC);
CREATE INDEX IF NOT EXISTS idx_ops_cache_analysis_5m_user_bucket
    ON ops_cache_analysis_5m (user_id, bucket_start DESC);

COMMENT ON TABLE ops_cache_analysis_5m IS 'Prompt-cache hit/miss root causes per 5-minute bucket; additive upserts from every gateway process.';
