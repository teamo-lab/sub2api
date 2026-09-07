WITH usage_rows AS (
    SELECT
        CASE
            WHEN NULLIF(request_id, '') IS NOT NULL
                THEN request_id || ':' || api_key_id::text
            ELSE 'usage:' || id::text
        END AS request_key,
        request_id,
        api_key_id,
        (NOT stream OR first_token_ms IS NOT NULL) AS succeeded,
        first_token_ms,
        duration_ms,
        input_tokens::bigint AS input_tokens,
        cache_read_tokens::bigint AS cache_read_tokens,
        cache_creation_tokens::bigint AS cache_creation_tokens,
        actual_cost::numeric AS actual_cost
    FROM usage_logs
    WHERE deployment_slot = :'slot'
      AND created_at >= :'window_start'::timestamptz
      AND created_at < :'window_end'::timestamptz
      AND (NULLIF(:'api_key_id', '') IS NULL OR api_key_id = NULLIF(:'api_key_id', '')::bigint)
      -- cyber-policy denials deliberately write a zero-token usage row so they
      -- remain auditable, but request_type=4 is a business-limited outcome and
      -- must not re-enter SLA through usage_rows after its ops error is excluded.
      AND request_type <> 4
), error_source_rows AS (
    SELECT
        error.*,
        CASE
            WHEN NULLIF(error.client_request_id, '') IS NOT NULL
                THEN CASE
                    WHEN error.client_request_id LIKE 'client:%'
                        THEN error.client_request_id
                    ELSE 'client:' || error.client_request_id
                END || ':' || COALESCE(error.api_key_id, 0)::text
            WHEN NULLIF(error.request_id, '') IS NOT NULL
                THEN error.request_id || ':' || COALESCE(error.api_key_id, 0)::text
            ELSE 'error:' || error.id::text
        END AS request_key
    FROM ops_error_logs error
    WHERE error.deployment_slot = :'slot'
      AND error.created_at >= :'window_start'::timestamptz
      AND error.created_at < :'window_end'::timestamptz
      AND (NULLIF(:'api_key_id', '') IS NULL OR error.api_key_id = NULLIF(:'api_key_id', '')::bigint)
      AND NOT error.is_business_limited
      AND NOT error.is_count_tokens
), terminal_error_rows AS (
    SELECT error.*
    FROM error_source_rows error
    WHERE COALESCE(error.status_code, 0) >= 400
       OR (
            error.error_owner = 'provider'
            AND COALESCE(error.error_type, '') <> ''
            -- status<400 rows explicitly emitted as recovered-attempt telemetry
            -- are never terminal outcomes. This also covers a client that
            -- disconnects after a failed attempt but before replay completes.
            AND COALESCE(error.error_message, '') NOT LIKE 'Recovered upstream error%'
            AND COALESCE(error.error_message, '') NOT LIKE 'Recovered account authentication failure%'
            -- A recovered status-200 upstream attempt has a final usage row and
            -- must not lower SLA.  A committed SSE failure has no usage row;
            -- count it even if an older producer accidentally persisted the
            -- HTTP wire status (200) instead of the intended logical status.
            AND NOT EXISTS (
                SELECT 1
                FROM usage_rows usage
                WHERE usage.request_key = error.request_key
            )
       )
), error_rows AS (
    SELECT
        request_key,
        false AS succeeded
    FROM terminal_error_rows
), outcomes AS (
    SELECT request_key, bool_and(succeeded) AS succeeded
    FROM (
        SELECT request_key, succeeded FROM usage_rows
        UNION ALL
        SELECT request_key, succeeded FROM error_rows
    ) terminal_rows
    GROUP BY request_key
), usage_metrics AS (
    SELECT
        COUNT(first_token_ms)::bigint AS ttft_samples,
        percentile_cont(0.50) WITHIN GROUP (ORDER BY first_token_ms)
            FILTER (WHERE first_token_ms IS NOT NULL)::double precision AS ttft_p50_ms,
        percentile_cont(0.95) WITHIN GROUP (ORDER BY first_token_ms)
            FILTER (WHERE first_token_ms IS NOT NULL)::double precision AS ttft_p95_ms,
        percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms)
            FILTER (WHERE duration_ms IS NOT NULL)::double precision AS duration_p50_ms,
        percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
            FILTER (WHERE duration_ms IS NOT NULL)::double precision AS duration_p95_ms,
        COALESCE(SUM(input_tokens), 0)::bigint AS input_tokens,
        COALESCE(SUM(cache_read_tokens), 0)::bigint AS cache_read_tokens,
        COALESCE(SUM(cache_creation_tokens), 0)::bigint AS cache_creation_tokens,
        COALESCE(SUM(actual_cost), 0)::double precision AS actual_cost_usd
    FROM usage_rows
), error_metrics AS (
    SELECT
        COUNT(*) FILTER (WHERE COALESCE(upstream_status_code, status_code, 0) = 429)::bigint AS upstream_429,
        COUNT(*) FILTER (WHERE COALESCE(upstream_status_code, status_code, 0) = 529)::bigint AS upstream_529,
        COUNT(*) FILTER (
            WHERE COALESCE(upstream_status_code, status_code, 0) BETWEEN 500 AND 599
               OR (COALESCE(status_code, 0) < 400 AND error_owner = 'provider')
        )::bigint AS upstream_5xx
    FROM terminal_error_rows
), outcome_metrics AS (
    SELECT
        COUNT(*)::bigint AS terminal_outcomes,
        COUNT(*) FILTER (WHERE succeeded)::bigint AS successes,
        COUNT(*) FILTER (WHERE NOT succeeded)::bigint AS failures
    FROM outcomes
)
SELECT json_build_object(
    'slot', :'slot',
    'window_start', :'window_start',
    'window_end', :'window_end',
    'terminal_outcomes', outcome_metrics.terminal_outcomes,
    'successes', outcome_metrics.successes,
    'failures', outcome_metrics.failures,
    'ttft_samples', usage_metrics.ttft_samples,
    'ttft_p50_ms', usage_metrics.ttft_p50_ms,
    'ttft_p95_ms', usage_metrics.ttft_p95_ms,
    'duration_p50_ms', usage_metrics.duration_p50_ms,
    'duration_p95_ms', usage_metrics.duration_p95_ms,
    'input_tokens', usage_metrics.input_tokens,
    'cache_read_tokens', usage_metrics.cache_read_tokens,
    'cache_creation_tokens', usage_metrics.cache_creation_tokens,
    'actual_cost_usd', usage_metrics.actual_cost_usd,
    'upstream_429', error_metrics.upstream_429,
    'upstream_529', error_metrics.upstream_529,
    'upstream_5xx', error_metrics.upstream_5xx
)
FROM outcome_metrics, usage_metrics, error_metrics;
