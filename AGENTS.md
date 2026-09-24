# About scheduler

## What it owns

Centralized job scheduler with cron-based scheduling, SQLite storage, REST API, and Google Calendar integration. Two job types: **shell** (executes `sh -c <command>`) and **agent** (invokes a Claude-based agent with a prompt). Schedules use standard 5-field cron (robfig/cron).

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Common calls

```bash
# List jobs
curl -s http://localhost:8092/api/jobs

# Create a shell job (e.g. poke an HTTP endpoint every 5 min)
curl -s -X POST http://localhost:8092/api/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "disk-root health check",
    "description": "Hits healthcheck /api/check/disk-root",
    "schedule": "*/5 * * * *",
    "type": "shell",
    "command": "curl -sfS -X POST http://localhost:8099/api/check/disk-root"
  }'

# Create an agent job
curl -s -X POST http://localhost:8092/api/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "daily summary",
    "schedule": "0 8 * * *",
    "type": "agent",
    "agent": "claude-code",
    "prompt": "Summarize yesterday's activity…"
  }'

# Run a job immediately
curl -s -X POST http://localhost:8092/api/jobs/<id>/run

# Enable/disable
curl -s -X PATCH http://localhost:8092/api/jobs/<id> \
  -H "Content-Type: application/json" -d '{"enabled":false}'

# Give a long-running job its own wall-clock cap (0 = the 5-minute default)
curl -s -X PATCH http://localhost:8092/api/jobs/<id> \
  -H "Content-Type: application/json" -d '{"timeout_seconds":3600}'

# Edit any other field — prompt, schedule, command, agent, model, workspace_id …
curl -s -X PATCH http://localhost:8092/api/jobs/<id> \
  -H "Content-Type: application/json" -d '{"prompt":"…","schedule":"0 6 * * *"}'

# Hold (pause) a job until a future time
curl -s -X POST http://localhost:8092/api/jobs/<id>/hold \
  -H "Content-Type: application/json" -d '{"days":7}'     # or {"until":"2026-05-01T00:00:00Z"}
curl -s -X DELETE http://localhost:8092/api/jobs/<id>/hold  # clear hold

# Run history (status, output, timing per run)
curl -s http://localhost:8092/api/jobs/<id>/runs

# Delete job
curl -s -X DELETE http://localhost:8092/api/jobs/<id>
```

## Timeouts, and a slow job never stacks

A shell job's wall-clock cap is **per-job**: `timeout_seconds` on create or PATCH, `0` meaning the 5-minute default. Set it generously for anything that legitimately runs long (`harness-watch` is 1800; four of the six nightly `repo-*` guards are 3600, and `repo-elf-guard` and `repo-nul-guard` are 600 — re-measured 2026-08-21, when the count was still written as five) — the cap is enforced by killing the job's whole process group, so a job that outlives it is killed, not left to run. A tick that arrives while the previous run of that same job is **still in flight is dropped, not stacked**, so a slow job can no longer pile up concurrent copies of itself.

## PATCH takes every field and refuses a bad one

`PATCH` takes **every** job field, not just `enabled`. Send only the fields you are changing. It refuses rather than half-obeys: a misspelled field name, an unparseable cron expression, or an edit that would leave the job unrunnable (a shell job with no command, an agent job with no prompt) is a 400. A successful edit reloads the schedule, so it takes effect on the **next tick** — editing the DB by hand and forcing a reload with a no-op PATCH is no longer needed, and the row you edit by hand is now also the row the next run executes.

**Re-measured live 2026-08-13 on a disposable job, and this is now true of the running service too.** For five days this section carried a ⛔ block saying the opposite — that the deployed binary silently dropped every field but three and answered `200` — on the strength of a 2026-08-08 measurement plus the binary's build date. `~/bin/scheduler` was rebuilt **2026-08-11 20:33**, after commit `863868a` landed, and nobody re-measured. Verified this time by creating a throwaway job and reading it back: `PATCH {"schedule":"0 6 * * *","command":"echo CHANGED"}` → the row really changed; `{"scheduel":…}` → **400** `unknown field`; `{"schedule":"not a cron"}` → **400**; `{"command":""}` on a shell job → **400** `command is required for shell jobs`. The DB-surgery workaround is obsolete, as originally written. **A build date is not a deploy check — re-measure.**

## A job cannot be created disabled

⚠️ **You cannot create a disabled job through the API.** The `POST /api/jobs` request struct has no `enabled` field at all (`internal/api/api.go`) and `db.go` sets `Enabled = true` on insert. The strict decoder rejects `{"enabled":false}` with a **400** `json: unknown field "enabled"` (measured live 2026-08-13), so you learn it failed rather than getting a live cron job you believed was off. Create it, then PATCH `{"enabled":false}`.

## Run statuses

Shell jobs return status `success` or `error` based on exit code; `output` field in run history captures combined stdout+stderr (use `-f` with curl so 5xx becomes non-zero exit → `error` status → visible in `/runs`). Two further statuses mean the run did not finish on its own terms: `timeout` (it exceeded `timeout_seconds` and its process group was killed) and `interrupted` (the scheduler process died mid-run — a crash, an OOM kill, a deploy restart — and the run was closed out at the next startup). A run is never left in `running` by a process that is gone.

## email-classifier's model calls are operations

Every model call `email-classifier` makes — filing, the merge pass, the deadline backfill — is an `llm.completion` operation on llm-bridge-server (`internal/completionoperation`), not a call to an instance's oneshot endpoint. Each is made for the principal-store group in the board's `classifier.organization_id`, so that organization's monthly budget and grants apply; a board without one stops the run before the first call. The bridge picks the harness instance (its `operations.completion_instance` setting), so the binary takes no instance flag. A run's calls share its run id as correlation id, and each gets the idempotency key `<run id>:<call number>`. `completionoperation.ErrTruncated` and `ErrBudgetExhausted` name the two failures a caller must not retry blindly. Filing and the deadline backfill ask for `-model` (`claude-sonnet-4-6`); the card-merge pass asks for `-merge-model`, default model-store's `efficient` role, which the bridge resolves and routes to its provider's instance. The merge pass sends the whole board in every call but asks about fifty cards a merge could touch at a time, four calls at once, and joins the groups the calls propose before applying any (`merge_batches.go`). `kanban-classifier`, `kanban-scoper` and `demo-mail-generator` still use `internal/oneshot`.

## Mail becomes a ticket

`email-classifier` gives a card it files for a piece of work a **ticket** in
kanban-store: the requester and the channel. The requester is a principal-store
**contact id**, resolved from the sender's address through
`POST /contacts/resolve` (`PRINCIPAL_STORE_URL`), never the address itself —
one person writes from a phone, a work account and a web form.

Two cards deliberately stay plain cards: a **bucket** card in the *No action*
column, which collects mail from many senders and so has no one requester, and
a card that already has a ticket, because the first requester stands.

A card the classifier did not create becomes a ticket too, the first time mail
lands on it. Most mail joins a card that already exists — the first live run
filed two messages and created no card — so writing tickets only for new cards
would leave a board's existing work without requesters for as long as those
cards live. The board view says which cards already have one, so this costs no
extra read.

A failure writes no ticket and does not stop the filing: the card is correct
without it, and refusing to file mail because principal-store was restarting
would lose both. The run summary counts `tickets_written` and `tickets_failed`.

**There is no ticket status.** The lifecycle is the column the card sits in;
classify a board's columns with `lifecycle_state` (kanban-store's
`GET /api/ticket-lifecycle-states`) and moving a card is what moves its ticket.

## Reminders: one coordinator, never a new cron job

`~/bin/reminder-coordinator` (scheduler job 32, `*/15`) is the **only** thing that reminds the user of anything. It materializes recurring todos, rewrites the standing **"📆 Today — personal"** note (the primary surface — always current, readable at dash `/notes`), mirrors opted-in items to Google Calendar, and sends **at most one herald session per tick**.

It replaced five jobs that each nudged without knowing what the others had said, so an overdue todo with its own nudge job opened two chat sessions a day.

**Do not add a cron job to nudge about a todo.** That is what recreated the bug. Put the cadence on the todo as `schedule.remind.nag` and the coordinator will deliver it in its single session. Source: `~/repos/scheduler/cmd/reminder-coordinator/`.

## Three things called a schedule

**Three unrelated things are called a "schedule" — say which you mean.** (1) cron occurrences from scheduler `:8092`, drawn on `/calendar` as the read-only "Cron" source; (2) an RFC 5545 `RRULE` on a Google event, which dash proxies through untouched; (3) `cadence_hours` on an event-store source, meaning how often to re-scrape it. Recurring *todos* are a fourth thing again — noteboard `schedule` (see CLAUDE.md).

# Access and operations

## Who may call it

Centralized cron scheduler on `localhost:8092`. Auth via `Authorization: Bearer $SCHEDULER_TOKEN` only if the server was started with `SCHEDULER_TOKEN` set (currently unset on this host — omit header). Jobs are stored in SQLite at `~/.scheduler/scheduler.db`. `GET /settings` (at the root, not under `/api`) lists every variable the service reads and the value in force; the table is in the README, the declarations in `internal/config/settings.go`. ⚠️ A set variable starting with `SCHEDULER_PORT`, `SCHEDULER_DB` or `SCHEDULER_TOKEN` that is not declared stops the service, and with it every job: `deploy.sh` checks the running service's environment before it stops it. The job binaries in `cmd/` declare their own variables in `internal/config/jobs.go`, one registry each; a set variable under a name one owns (`AUTOWORKER_`, `EMAIL_CLASSIFIER_`, `REMINDER_`, …) that it does not declare stops that job, and the same `deploy.sh` check builds every job's registry from the live environment too.
