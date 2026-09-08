-- Profiles reuse the bounded asynchronous ops log sink and its retention policy.
CREATE INDEX IF NOT EXISTS idx_ops_request_profile_created
ON ops_system_logs (created_at DESC, id DESC)
WHERE extra ? 'request_profile' AND component = 'http.access' AND message = 'http request completed';
