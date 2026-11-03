CREATE TABLE ${TABLE_PREFIX}jobs (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    tags JSONB NOT NULL DEFAULT '[]',
    schedule TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    executor TEXT NOT NULL,
    config JSONB NOT NULL DEFAULT '{}',
    concurrency TEXT NOT NULL DEFAULT 'forbid',
    catch_up BOOLEAN NOT NULL DEFAULT FALSE,
    timeout_seconds INTEGER NOT NULL DEFAULT 0,
    retries INTEGER NOT NULL DEFAULT 0,
    retry_delay_seconds INTEGER NOT NULL DEFAULT 10,
    next_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX ${TABLE_PREFIX}jobs_due ON ${TABLE_PREFIX}jobs (next_run_at) WHERE enabled;

CREATE TABLE ${TABLE_PREFIX}runs (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES ${TABLE_PREFIX}jobs(id) ON DELETE CASCADE,
    trigger TEXT NOT NULL,
    status TEXT NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 1,
    scheduled_at TIMESTAMPTZ NOT NULL,
    not_before TIMESTAMPTZ NOT NULL,
    node TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    result_code INTEGER,
    output TEXT NOT NULL DEFAULT '',
    output_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    error TEXT NOT NULL DEFAULT '',
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE
);

-- One scheduled run per job and time, whichever replica creates it.
CREATE UNIQUE INDEX ${TABLE_PREFIX}runs_scheduled ON ${TABLE_PREFIX}runs (job_id, scheduled_at) WHERE trigger = 'schedule';
CREATE INDEX ${TABLE_PREFIX}runs_job ON ${TABLE_PREFIX}runs (job_id, id DESC);
CREATE INDEX ${TABLE_PREFIX}runs_queued ON ${TABLE_PREFIX}runs (not_before) WHERE status = 'queued';
CREATE INDEX ${TABLE_PREFIX}runs_active ON ${TABLE_PREFIX}runs (job_id) WHERE status IN ('queued', 'running');
CREATE INDEX ${TABLE_PREFIX}runs_finished ON ${TABLE_PREFIX}runs (finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE ${TABLE_PREFIX}nodes (
    name TEXT PRIMARY KEY,
    version TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL,
    seen_at TIMESTAMPTZ NOT NULL
);
