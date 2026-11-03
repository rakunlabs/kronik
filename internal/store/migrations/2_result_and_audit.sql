ALTER TABLE ${TABLE_PREFIX}runs ADD COLUMN result TEXT NOT NULL DEFAULT '';
-- Who queued or canceled a manual run, from the proxy's user header.
ALTER TABLE ${TABLE_PREFIX}runs ADD COLUMN triggered_by TEXT NOT NULL DEFAULT '';
ALTER TABLE ${TABLE_PREFIX}runs ADD COLUMN canceled_by TEXT NOT NULL DEFAULT '';

ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN updated_by TEXT NOT NULL DEFAULT '';

-- Every change made through the API or MCP, with who made it.
CREATE TABLE ${TABLE_PREFIX}audit (
    id TEXT PRIMARY KEY,
    at TIMESTAMPTZ NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    via TEXT NOT NULL DEFAULT 'api',
    action TEXT NOT NULL,
    job_id TEXT NOT NULL DEFAULT '',
    job_name TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL DEFAULT '',
    detail JSONB NOT NULL DEFAULT '{}'
);

CREATE INDEX ${TABLE_PREFIX}audit_at ON ${TABLE_PREFIX}audit (at DESC);
CREATE INDEX ${TABLE_PREFIX}audit_job ON ${TABLE_PREFIX}audit (job_id, at DESC);
