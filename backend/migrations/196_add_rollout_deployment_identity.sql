-- Stamp blue-green deployment identity through PostgreSQL session defaults.
-- Existing/legacy processes do not set these GUCs and therefore keep NULL,
-- preserving compatibility while a release contains both old and new code.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS deployment_slot VARCHAR(32),
    ADD COLUMN IF NOT EXISTS release_id VARCHAR(128),
    ADD COLUMN IF NOT EXISTS deployment_version VARCHAR(128),
    ADD COLUMN IF NOT EXISTS deployment_digest VARCHAR(71);

-- Add nullable columns without defaults first. This is a metadata-only expand
-- migration even on large tables. Setting defaults separately affects future
-- rows only and avoids evaluating current_setting() for every historical row.
ALTER TABLE usage_logs
    ALTER COLUMN deployment_slot SET DEFAULT NULLIF(current_setting('sub2api.deployment_slot', true), ''),
    ALTER COLUMN release_id SET DEFAULT NULLIF(current_setting('sub2api.release_id', true), ''),
    ALTER COLUMN deployment_version SET DEFAULT NULLIF(current_setting('sub2api.deployment_version', true), ''),
    ALTER COLUMN deployment_digest SET DEFAULT NULLIF(current_setting('sub2api.deployment_digest', true), '');

ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS deployment_slot VARCHAR(32),
    ADD COLUMN IF NOT EXISTS release_id VARCHAR(128),
    ADD COLUMN IF NOT EXISTS deployment_version VARCHAR(128),
    ADD COLUMN IF NOT EXISTS deployment_digest VARCHAR(71);

ALTER TABLE ops_error_logs
    ALTER COLUMN deployment_slot SET DEFAULT NULLIF(current_setting('sub2api.deployment_slot', true), ''),
    ALTER COLUMN release_id SET DEFAULT NULLIF(current_setting('sub2api.release_id', true), ''),
    ALTER COLUMN deployment_version SET DEFAULT NULLIF(current_setting('sub2api.deployment_version', true), ''),
    ALTER COLUMN deployment_digest SET DEFAULT NULLIF(current_setting('sub2api.deployment_digest', true), '');
