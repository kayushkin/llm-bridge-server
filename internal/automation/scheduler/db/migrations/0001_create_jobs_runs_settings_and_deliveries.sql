-- The scheduler's jobs, every run of them, the settings it stores, and which
-- bus message started which run of a triggered job.

-- A job's public id is scheduler_job_ and its number, as reference-store
-- shows it; the API serves the number. event_trigger is the trigger as JSON,
-- '' for a job that runs on its schedule.
CREATE TABLE jobs (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            text NOT NULL,
    description     text NOT NULL DEFAULT '',
    schedule        text NOT NULL,
    command         text NOT NULL DEFAULT '',
    type            text NOT NULL DEFAULT 'shell',
    agent           text NOT NULL DEFAULT '',
    prompt          text NOT NULL DEFAULT '',
    model_role      text NOT NULL DEFAULT '',
    orchestrator    text NOT NULL DEFAULT '',
    session_id      text NOT NULL DEFAULT '',
    workspace_id    text NOT NULL DEFAULT '',
    event_trigger   text NOT NULL DEFAULT '',
    timeout_seconds integer NOT NULL DEFAULT 0,
    enabled         boolean NOT NULL DEFAULT true,
    hold_until      timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);

-- A run keeps its job's id after the job is deleted, as it always has: the
-- SQLite table declared a cascade that SQLite never enforced, and the newest
-- run of a deleted job is still answered by GET /api/runs/latest. So job_id
-- has no foreign key. output is the bytes the command wrote, which need not be
-- text.
CREATE TABLE runs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id      bigint NOT NULL,
    status      text NOT NULL DEFAULT 'running',
    output      bytea NOT NULL DEFAULT ''::bytea,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz
);

-- Every read of runs is by job, newest first.
CREATE INDEX runs_job_id_id ON runs (job_id, id);
-- Startup finds the runs a stopped process left going.
CREATE INDEX runs_still_running ON runs (id) WHERE status = 'running';

-- The settings the service stores: the model role each job binary asks for.
CREATE TABLE service_settings (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One row per (job, message) that started a run, so a message delivered twice
-- starts each job once.
CREATE TABLE event_trigger_deliveries (
    job_id          bigint NOT NULL,
    idempotency_key text NOT NULL,
    run_id          bigint NOT NULL,
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (job_id, idempotency_key)
);
