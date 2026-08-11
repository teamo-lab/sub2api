CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_deployment_slot_created_at
    ON usage_logs (deployment_slot, created_at DESC)
    WHERE deployment_slot IS NOT NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_error_logs_deployment_slot_created_at
    ON ops_error_logs (deployment_slot, created_at DESC)
    WHERE deployment_slot IS NOT NULL;
