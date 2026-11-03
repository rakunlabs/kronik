-- Jobs: random delay, "did not run" alert, notifications, webhook trigger.
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN jitter_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN alert_after_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN alerted_at TIMESTAMPTZ;
-- health is '' (no result yet), 'ok' or 'failing': what the last final run
-- (or the "did not run" alert) left, so a notification goes on a change.
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN health TEXT NOT NULL DEFAULT '';
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN notify JSONB NOT NULL DEFAULT '{}';
-- SHA-256 of the job's webhook token; '' when the job has none.
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN hook_hash TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX ${TABLE_PREFIX}jobs_hook ON ${TABLE_PREFIX}jobs (hook_hash) WHERE hook_hash <> '';
-- Run this job when one of these jobs' runs ends (after_on: succeeded,
-- failed or finished).
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN after_jobs JSONB NOT NULL DEFAULT '[]';
ALTER TABLE ${TABLE_PREFIX}jobs ADD COLUMN after_on TEXT NOT NULL DEFAULT 'succeeded';

-- Values given to one run by hand or by a webhook.
ALTER TABLE ${TABLE_PREFIX}runs ADD COLUMN params JSONB NOT NULL DEFAULT '{}';

-- Secrets: each value is encrypted with a data key; data keys are encrypted
-- with the master key from the environment and rotated from the settings.
CREATE TABLE ${TABLE_PREFIX}secret_keys (
    id TEXT PRIMARY KEY,
    wrapped BYTEA NOT NULL,
    -- master is the id of the master key that wraps this one.
    master TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX ${TABLE_PREFIX}secret_keys_active ON ${TABLE_PREFIX}secret_keys (active) WHERE active;

CREATE TABLE ${TABLE_PREFIX}secrets (
    name TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    value BYTEA NOT NULL,
    key_id TEXT NOT NULL REFERENCES ${TABLE_PREFIX}secret_keys (id),
    created_at TIMESTAMPTZ NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

-- Where notifications go: Slack, Teams, a webhook or e-mail.
CREATE TABLE ${TABLE_PREFIX}channels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    config JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);

-- Notifications to send, written with the run result that causes them and
-- sent by any replica.
CREATE TABLE ${TABLE_PREFIX}notifications (
    id TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES ${TABLE_PREFIX}channels (id) ON DELETE CASCADE,
    job_id TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL DEFAULT '',
    event TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ,
    error TEXT NOT NULL DEFAULT ''
);

CREATE INDEX ${TABLE_PREFIX}notifications_due ON ${TABLE_PREFIX}notifications (next_at) WHERE status = 'pending';
CREATE INDEX ${TABLE_PREFIX}notifications_created ON ${TABLE_PREFIX}notifications (created_at DESC);
CREATE INDEX ${TABLE_PREFIX}notifications_job ON ${TABLE_PREFIX}notifications (job_id, created_at DESC);
