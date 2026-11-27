-- 按 router uid 查某个用户的错误、按 router request_id 精确对单；只索引带值的行，
-- CONCURRENTLY 避免阻塞 ops_error_logs 写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_error_logs_router_user_time
    ON ops_error_logs (router_user_id, created_at DESC)
    WHERE router_user_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_error_logs_router_request_id
    ON ops_error_logs (router_request_id)
    WHERE router_request_id IS NOT NULL;
