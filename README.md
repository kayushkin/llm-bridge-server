# Scheduler

Centralized job scheduler for the inber ecosystem.

## Architecture
- Go service with robfig/cron for scheduling
- SQLite for job storage + run history
- REST API for management
- Google Calendar API integration

## API
- `GET /api/jobs` — list jobs
- `POST /api/jobs` — create job
- `GET /api/jobs/:id` — one job
- `PATCH /api/jobs/:id` — edit any job field. Send only the fields you are
  changing; the rest are left alone. A field the API does not recognise, an
  unparseable cron expression, or an edit that would leave the job unrunnable
  (a shell job with no command, an agent job with no prompt) is a 400 — nothing
  is accepted and then quietly dropped. A successful edit reloads the cron
  schedule, so it takes effect on the next tick rather than the next restart.
- `POST /api/jobs/:id/hold` / `DELETE /api/jobs/:id/hold` — pause a job until a
  future time, or clear the hold
- `DELETE /api/jobs/:id` — delete job
- `GET /api/jobs/:id/runs` — run history
- `POST /api/jobs/:id/run` — trigger now
- `GET /api/calendar/events` — Google Calendar events
- `POST /api/calendar/events` — create calendar event
- `GET /settings` — the service's settings (llm-bridge `servicesettings`): each
  variable, its value in force and where the value came from, a secret's value
  withheld. At the root, not under `/api`, and behind the same bearer.
- `PUT /settings/{key}` with `{"value":"balanced"}` — change a stored setting:
  the model role a job binary asks for. 409 for a setting read where the
  process starts, 400 for a role model-store does not list, 502 while
  model-store cannot be asked.

## Configuration

The service (`cmd/scheduler`) reads these, declared once in
`internal/config/settings.go`. A test fails on any `os.Getenv` in the service's
packages that the declarations do not name. A set variable starting with
`SCHEDULER_PORT`, `SCHEDULER_DB` or `SCHEDULER_TOKEN` that is not declared (a
misspelling such as `SCHEDULER_DB_PATH`) stops the start; `SCHEDULER_URL`, which
jobs read to find the scheduler, is not the service's and is left alone.

| variable | default | |
|---|---|---|
| `SCHEDULER_PORT` | `8092` | a whole number, or the start is refused |
| `SCHEDULER_DB` | `~/.scheduler/scheduler.db` | required; with no home directory the start is refused |
| `SCHEDULER_TOKEN` | unset | the bearer for every route but `/health` |
| `NATS_URL` | `nats://localhost:4222` | the service does not start without it |
| `LLMBRIDGE_URL` | `http://localhost:8160` | where agent jobs find a harness instance |
| `NOTEBOARD_URL` | `http://localhost:8191` | written into an agent job's working-memory preamble |
| `MODEL_STORE_URL` | `http://localhost:8155` | where a new model role is checked (`GET /api/roles`) before it is stored |

### Model roles: stored by the service, read by the jobs

Each job binary that calls a model asks for a model-store **role** (`best`,
`default`, `balanced`, `efficient`), never a model or an instance. The
scheduler service stores the role in its database (`service_settings`), shows
it at `GET /settings` and changes it at `PUT /settings/{key}`; dash's settings
page reaches both through `/api/scheduler/settings`. The binary reads its role
from `GET $SCHEDULER_URL/settings` each time it starts, so a change takes
effect on its next run. No flag or variable of a binary names a model: an old
one such as `KANBAN_SCOPER_MODEL`, left set, stops the binary.

The `SCHEDULER_…_MODEL_ROLE` variable seeds the stored value once, on the first
start after the setting exists; after that it is ignored, and `GET /settings`
says so in the setting's notes.

| key (variable to seed it) | seeded with | asked for before | used by |
|---|---|---|---|
| `email_classifier_model_role` (`SCHEDULER_EMAIL_CLASSIFIER_MODEL_ROLE`) | `default` | `claude-sonnet-4-6` | email-classifier filing and deadline backfill, as `llm.completion` operations |
| `email_classifier_merge_model_role` (`SCHEDULER_EMAIL_CLASSIFIER_MERGE_MODEL_ROLE`) | `balanced` | `balanced` | email-classifier card-merge pass, as `llm.completion` operations |
| `kanban_classifier_model_role` (`SCHEDULER_KANBAN_CLASSIFIER_MODEL_ROLE`) | `efficient` | `claude-haiku-4-5-20251001` on `-oneshot-instance` | kanban-classifier, through the bridge's `POST /oneshot` |
| `kanban_scoper_model_role` (`SCHEDULER_KANBAN_SCOPER_MODEL_ROLE`) | `efficient` | `claude-haiku-4-5-20251001` on `-oneshot-instance` | kanban-scoper, through `POST /oneshot` |
| `demo_mail_generator_model_role` (`SCHEDULER_DEMO_MAIL_GENERATOR_MODEL_ROLE`) | `default` | `claude-sonnet-4-6` on `inst-cc-local` | demo-mail-generator, through `POST /oneshot` |

`POST /oneshot` tries the role's models in order, each on the instance that
serves its provider, and names every attempt when all fail. An `llm.completion`
operation calls the role's first model only, and holds the call to the board
organization's budget.

The twelve job binaries in `cmd/` (`ask`, `autoworker`, `email-classifier`,
`reminder-coordinator` and the rest) serve no HTTP, so they are not on the
settings page. Each declares what it reads in `internal/config/jobs.go` and
builds its own registry at start; a test holds every environment read in the
binary and the packages it imports to that list. A binary owns only the names
that are its alone — `AUTOWORKER_`, `DEMO_MAIL_`, `EMAIL_CLASSIFIER_` and
`EMAIL_BOARD_ID`, `KANBAN_CLASSIFIER_`, `KANBAN_SCOPER_`, `REMINDER_` — and a set
variable under one of them that it does not declare stops it with exit 1. The
shared addresses and the two service tokens are declared and owned by nobody.


## Autoworker kanban contract

The autoworker dispatches budgeted agent fires using two kanban-store boards.
Both follow the same column shape (`Queued / In Progress / Blocked / Done /
Failed`); `Done` carries `auto_status=done` and `Failed` carries
`auto_status=archived`, so kanban moves keep the underlying noteboard item
status aligned with the terminal column.

| Board                 | Owner               | Role                                                                 |
|-----------------------|---------------------|----------------------------------------------------------------------|
| `Autoworker queue`    | `cmd/autoworker`    | Dispatch source. The worker selects the head of `Queued`, attaches a `bus_session` link, and moves the card to `In Progress`. |
| `Agent runs`          | `cmd/autoworker`    | Per-fire telemetry card created on each dispatch (provider, harness, instance, queue card id, target/actual). |

The `cmd/kanban-curator` job watches `In Progress` cards on a single board and
moves them when the linked bridge session reaches a terminal state. Each board
that needs closure registers its own curator cron job:

```bash
# Default — closes the "Agent runs" telemetry board
kanban-curator -register

# Closes "Autoworker queue" cards once their bridge session ends
kanban-curator -register -board="Autoworker queue"

# Closes "Task completion loop" board
kanban-curator -register -board="Task completion loop"
```

Without `-job-name`, the registered job name is `kanban-curator-tick` for the
default board, or `kanban-curator-<slug>-tick` derived from the board name
otherwise. The registered command preserves the `-board="..."` flag so each
tick targets the right board. Re-running `-register` is idempotent — it skips
when a job with the same name already exists.

### Verification paths (no live fire required)

- `autoworker -dry-run` — evaluates fire decisions for every provider and, on
  any tick where no provider fires, logs the head card of `Autoworker
  queue → Queued` (selection observability).
- `kanban-curator -dry-run -board="Autoworker queue"` — lists every `In
  Progress` card with a `bus_session` link, shows the bridge session state, and
  reports which terminal column it would move to without writing.
