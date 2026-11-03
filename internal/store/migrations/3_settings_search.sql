-- Settings changed from the UI, shared by every replica. A key that is not
-- here uses the replica's configuration (file or environment).
CREATE TABLE ${TABLE_PREFIX}settings (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

-- Run search by time and by who started a run.
CREATE INDEX ${TABLE_PREFIX}runs_created ON ${TABLE_PREFIX}runs (created_at DESC);
CREATE INDEX ${TABLE_PREFIX}runs_triggered_by ON ${TABLE_PREFIX}runs (triggered_by) WHERE triggered_by <> '';
CREATE INDEX ${TABLE_PREFIX}audit_job_id ON ${TABLE_PREFIX}audit (job_id, id DESC);
