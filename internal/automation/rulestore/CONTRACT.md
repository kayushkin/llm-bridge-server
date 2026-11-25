# rule-store — API contract

`127.0.0.1:8324`, no auth, so loopback only. Module
`github.com/kayushkin/rule-store`, root package `rulestore`, binary
`cmd/rule-store` installed at `~/bin/rule-store`. SQLite at
`~/.config/rule-store/rule-store.db`. Timestamps are epoch seconds.

## What it is

Rules that react to a change in one service by doing something in another:
"when a session in llm-bridge-server changes, if these conditions hold, do
these effects". rule-store owns the rules, their revisions, who activated them
and every decision they made. It owns no subject — sessions stay in
llm-bridge-server and cards in kanban-store — and it keeps no clock: the
scheduler keeps cron.

**A rule runs in shadow or live mode.** A shadow match records the exact owner calls its effects would make and makes none. A live match is recorded as `applying` **before** its first call — so the run's uniqueness stops any second decision from calling again — then makes the calls in order with the service tokens, records each owner's answer on the run (`planned_effects[].result`: `status`, `body` cut at 16 KB, `error` when no answer came), and ends `applied`, or `failed` at the first answer outside 2xx; calls after it are not made and nothing retries. A run still `applying` when the process stops is marked `interrupted` at the next start and never re-run. Switch a rule to live only after `GET /rules/{id}/comparison` agrees with the job it replaces, and turn that job off in the same sitting.

## What stays out

- **Rules that touch only their own service** stay in that service: session
  renaming, turn classification and the budget stop in llm-bridge-server;
  card-only ticket rules in kanban-store.
- **The clock.** No cron here. The sweep only re-checks conditions that time
  alone can change.
- **Controllers and pipelines**: autoworker's spend pacing, reminder
  calendars, email filing, the dispatcher's grouping.
- **Fetching facts.** A condition reads the subject that changed, its age, its
  entity tags and its linked cards — never another record.

## The rule language

A definition is a flat list of conditions, **all** of which must hold, and an
ordered list of effects. No "or", no nesting, no expressions: a rule that
needs "or" is two rules. Kinds are served at `GET /vocabulary`. Unknown JSON
keys, and keys a kind ignores, are a 400.

| Condition | Fields | Holds when |
|---|---|---|
| `field_in` / `field_not_in` | `field` (one of `session_fields`), `values` | the session's field is / is not one of the values. `status_reason` is `status.reason`: why the state last changed, from llm-bridge's served list (`GET /session-taxonomy` → `state_reasons`) |
| `updated_before` | `age` (Go duration) | the session's `updated_at` is older than `age` |
| `session_tag_present` / `session_tag_absent` | `tag` | kanban-store's entity tags on `session/<id>` include / lack it |
| `linked_card_in_column` | `board_id`, `column_id` | a card linked to the session sits in that column. **Columns are ids, never names** |

| Effect | Fields | Owner call it plans |
|---|---|---|
| `move_linked_cards` | `board_id`, `to_column_id` | kanban-store `POST /api/cards/{id}/move` for each card a `linked_card_in_column` condition on the same board found — required, so a move never touches a card a person just closed |
| `add_session_tag` | `tag` | kanban-store `POST /api/entities/session/{id}/tags` |
| `send_session_message` | `text` | llm-bridge-server `POST /sessions/{id}/send`, with `client_request_id` = `rule-store:<revision>:<session>:<trigger key>` so a live send repeated after a crash is one message |
| `move_session_to_folder` | `folder` | llm-bridge-server `PUT /sessions/{id}/folder` |

Conditions run in vocabulary order: those read from rule-store's copy of the
session first, those that call kanban-store last, and the first false one
stops — so a rule that cannot match costs no call.

## When a rule decides

A rule decides **at most once each time a session enters a state.**
llm-bridge-server records no "entered this state at", so rule-store counts
state changes itself: `subjects.state_episode` is 1 on first sight and goes up
by one on every change of state. A run is unique on
`(rule_revision_id, subject, "<state>#<episode>")`. A frame that only bumps
`updated_at` decides nothing new; a session that goes `completed` → running →
`completed` is decided twice.

Two paths decide; the uniqueness makes them safe together:

- **The feed.** rule-store follows `GET /session-events` (SSE) as the internal
  service. Each `upsert` updates its copy and the worker checks every active
  rule against that session within moments.
- **The sweep**, every `RULE_STORE_SWEEP_INTERVAL` (1m): rules with an
  `updated_before` condition, against every session in the states their
  `field_in state` condition names (all sessions if none). Rules without a time
  condition are not swept.

Frames other than `hello`, `upsert` and `delete` change no session row: `signal` (a session's questions or notifications changed) is ignored, and a type rule-store has never seen is skipped and counted under `reactor.unknown_frames` in `/health` — never a reason to drop the feed, which would reconnect into the same frame and loop.

When the feed's `hello` says anything but `resume: replayed` — first
connection, a bridge restart, a gap — rule-store reads `GET /sessions` again,
replaces its copies, forgets sessions the bridge no longer has, and checks every
rule against every session in the background.

⚠️ **kanban-store sends no feed.** A card moved into a rule's column *after*
its session changed is not noticed until the session changes again or a time
rule sweeps it. The comparison's `missed_cards` shows how often that matters.

## Routes

| Method | Path | Notes |
|---|---|---|
| GET | `/health` | `{"status","counts":{rules,active_rules,subjects,runs},"reactor":{feed_connected,last_seed_at,last_sweep_at,runs_recorded_since_start,errors_since_start,last_error,…}}` |
| GET | `/settings` | every environment variable read, as llm-bridge `msg.ServiceSettings`; tokens hidden; nothing editable |
| GET | `/vocabulary` | sources, condition kinds, session fields, effect kinds, modes, rule states |
| GET | `/rules` | `?state=` → bare array, oldest first, without revisions |
| POST | `/rules` | `{name, description?, source, created_by}` → **201**, a draft |
| GET | `/rules/{id}` | the rule with `revisions` and `activations` |
| POST | `/rules/{id}/revisions` | `{definition, note?, created_by}` → **201**. Validated; immutable; does not activate. **409** on a retired rule |
| POST | `/rules/{id}/activate` | `{revision, mode, acted_by, reason}`. `mode` is `shadow` or `live`. Also switches an active rule to another revision. Then checks the rule once, in the background, against every session in the states it names, so a rule without a time condition still decides about sessions already in its state |
| POST | `/rules/{id}/pause` | `{acted_by, reason}`. **409** unless active |
| POST | `/rules/{id}/retire` | `{acted_by, reason}`. Final |
| POST | `/rules/{id}/preview` | `{revision}` or `{definition}`, `limit?` (50) → `{candidates, matched, matches:[{subject_id, trigger_key, facts, planned_effects}]}` against every session held now. Records nothing and is not permission. Calls kanban-store for every candidate that passes the cheap conditions, so it can take a while |
| GET | `/rules/{id}/comparison` | `?since=` (default: first activation). For the newest 500 runs, whether each planned effect's result is visible at the owner now (`reached`, `reached_at`, `lead_seconds` = reached − decided), and for each move, `missed_cards`: cards that reached the target column since then, linked to a session no run decided |
| GET | `/runs` | `?rule_id=&subject_id=&since=&limit=` (100) → newest first |
| GET | `/subjects/{type}/{id}` | rule-store's copy of a subject and its episode |
| GET | `/summary` | one read for an overview page: `{counts, reactor, rules:[rule with revisions, activations, runs_total, runs_last_24h, last_decided_at]}` |
| GET | `/activity` | the reactor's recent activity, newest first, kept in memory (1,000 entries, lost on restart; the journal keeps the same lines): feed connects and drops, full reads, sweeps with counts and timing, runs, errors. `?after=<seq>` returns only newer entries, for polling; `?kind=` one of feed, seed, sweep, run, error; `?limit=` (200) |

`*_by` fields must be principal-store ids (`principal_000001`); rule-store
cannot check one exists, but refuses a name or an email. Errors are
`{"error":…}`: **400** invalid, **404** missing, **409** wrong state or
already decided, **500** otherwise.

## Ids

`rule_000001`, `rule_revision_000001`, `rule_activation_000001`,
`rule_run_000001`. Nothing is deleted except a subject copy whose session the
bridge deleted; runs about it stay.
