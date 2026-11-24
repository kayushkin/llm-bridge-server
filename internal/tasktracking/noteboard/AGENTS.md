# About noteboard

## What it owns

Unified notes/todos/ranking-lists service (SQLite + FTS5, REST API on 127.0.0.1:8191 — `NOTEBOARD_LISTEN_HOST`, loopback by default, because no route asks for credentials). Markdown bodies, JSON tags, full-text search, rerank. Canonical backend for dash `/notes` page (proxied via `/api/noteboard/*`). Systemd user unit: `noteboard.service`. DB: `~/.noteboard/noteboard.db` (SQLite + FTS5). Single source of truth — do NOT reintroduce a filesystem-based notes tree.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Items, types and status

Every item has a `type` of `note`, `todo`, `rank`, or `workspace`. Title is required; body is markdown. Status defaults to `open`; use `done` to complete a todo and `archived` to archive one.

## Common calls

```bash
# List items (optionally filter by type/tag/status/list_id)
curl -s "http://localhost:8191/api/items?type=todo&status=open&limit=100"

# Create a todo
curl -s -X POST http://localhost:8191/api/items \
  -H "Content-Type: application/json" \
  -d '{"type":"todo","title":"Call plumber","tags":["home"],"priority":2}'

# Create a note with markdown body
curl -s -X POST http://localhost:8191/api/items \
  -H "Content-Type: application/json" \
  -d '{"type":"note","title":"Deploy playbook","body":"## Steps\n1. …","tags":["ops"]}'

# Mark a todo done
curl -s -X PATCH http://localhost:8191/api/items/<id> \
  -H "Content-Type: application/json" \
  -d '{"status":"done"}'

# Delete (reversible — stamps deleted_at, leaves status alone)
curl -s -X DELETE http://localhost:8191/api/items/<id>

# Undo the delete; comes back exactly as it was
curl -s -X POST http://localhost:8191/api/items/<id>/restore

# Every prior state of an item (update / delete / restore)
curl -s http://localhost:8191/api/items/<id>/revisions

# Full-text search
curl -s "http://localhost:8191/api/search?q=plumber&type=todo"

# All unique tags / named lists
curl -s http://localhost:8191/api/tags
curl -s http://localhost:8191/api/lists

# Reorder items in a ranked list
curl -s -X POST http://localhost:8191/api/items/rerank \
  -H "Content-Type: application/json" \
  -d '{"items":[{"id":"<id1>","rank":1.0},{"id":"<id2>","rank":2.0}]}'
```

## Deletion does not destroy data

**Deletion in this store does not destroy data.** `DELETE` is reversible: it stamps `deleted_at` and the row stays, dropping out of every read path (list, get, search, tags, lists, count). It does *not* touch `status` — archiving is a state you chose for a live item, deletion is the item being taken away, and collapsing the two means a restore can't tell them apart. `POST /api/items/{id}/restore` brings it back in exactly the state it was deleted in. Every update, delete, restore, hold, unhold and purge writes a snapshot to `item_revisions`, readable at `GET /api/items/{id}/revisions`. `?hard=true` still purges the row, but snapshots first — reach for it only for genuinely heavy data.

## An update can destroy data

⚠️ **An UPDATE can still destroy data, so read before you overwrite.** The snapshot is partial: it covers 8 of the 20 item columns (`title`, `body`, `tags`, `status`, `priority`, `list_id`, `parent_id`, `links`) and **not** `rank`, `due_at` or `schedule`. Overwrite an item's `rrule` and the old rule is gone from the database entirely — no revision holds it. `POST /api/items/rerank` writes `rank` with no snapshot at all.

## Held items are hidden by default

⚠️ **`GET /api/items` and `/api/search` hide held items by default.** Nothing in a default listing tells you so. `include_held=true` reveals them, and held-ness is inherited down `parent_id`. That default is deliberate — a hold is the user parking work behind a gate — so an unattended worker should leave it alone rather than widen its own search with it.

## Of these items, which match

`POST /api/items/query {"ids":[…],"tags":[…],"priorities":[…],"statuses":[…],"due_before":…,"sort":…,"limit":…,"offset":…,"include_items":true}` answers `{total, ids, items, missing_ids}`: of the named items, which match every filter, in what order, one page of them. It is for a caller that owns a set of ids and none of what the items say — kanban-store filters and sorts a board with it — and it changes nothing. Sorts are served at `GET /api/items/query-options`; ties fall back to the order the ids were sent in. **Held items are answered**, because the caller named them; `missing_ids` are the ids naming no live item, whatever the filter. An unknown field or sort is a 400. ⚠️ Two rules a change must not break, both learned from this host's data: **due dates are compared as instants** (`julianday()`), because `due_at` is stored with its writer's offset and `10:00-07:00` sorts before `12:00Z` as text; and **the join is a `CROSS JOIN`**, because as a plain `JOIN` SQLite made `items` the outer loop and re-parsed the whole id list once per item — minutes at 100% CPU for 9,506 ids, about 100 ms once fixed. `TestItemsQueryWalksTheIdsOnce` pins the plan. Also: `PATCH /api/items/{id}` honors `If-Match: "<updated_at>"` and answers 412 with the current item. README has both in full.

## Workspaces

A `workspace` is an agent's durable working memory: a timestamped markdown document a recurring job reads first and rewrites last on every run. It's a distinct type so it can never be mistaken for work to do (it must not pollute the todo queue) nor clutter the notes list. Scheduler agent jobs point at one via `workspace_id`, and the executor injects its location and contract into the prompt automatically. Use `parent_id` to split an overgrown workspace into children with a roll-up left behind, so the memory always stays small enough for a human to read in one sitting.

## Recurrence: the schedule rule

Any item can carry a `schedule`: the rule for when it is due and when it nudges. It is available on every item type, not just personal ones — `tag` / `exclude_tag` is what separates personal from coding todos, not the presence of a rule.

`due_at` stays the single source of truth for **when an item is next due**. `schedule` is the rule that *generates* that answer; `dtstart` is the series **anchor** (iCalendar sense), never the current due date.

```bash
curl -s -X POST http://localhost:8191/api/items \
  -H "Content-Type: application/json" \
  -d '{"type":"todo","title":"Water the plants","tags":["personal","home"],
       "schedule":{
         "dtstart":"2026-07-21T09:00:00-07:00",
         "tzid":"America/Los_Angeles",
         "rrule":"FREQ=MONTHLY;BYDAY=3TU",
         "mode":"instance",
         "remind":{"lead":["-P1D"],"channels":["digest","calendar"]}
       }}'

# Preview when a rule ACTUALLY fires — do this before trusting one.
curl -s "http://localhost:8191/api/items/<id>/occurrences?from=2026-07-01T00:00:00-07:00&to=2027-01-01T00:00:00-08:00"
```

- **`rrule`** — any RFC 5545 rule. `FREQ=MONTHLY;BYDAY=3TU` (every 3rd Tuesday), `FREQ=WEEKLY;INTERVAL=2;BYDAY=TH` (every other Thursday), `FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1` (last weekday of the month). End with `;COUNT=n` or `;UNTIL=...`. Use `rdate` for specific one-off dates and `exdate` to skip an occurrence.
- **`tzid`** — **mandatory** whenever a rule is present, and never defaulted. An offset is not a zone: a rule anchored to `-07:00` cannot know what happens after a DST transition and silently drifts an hour twice a year.
- **`mode`** — `instance` (default): each occurrence is materialized as a **child todo**, so downstream consumers just see ordinary todos and never need to understand recurrence. `rolling`: one row whose `due_at` advances when completed (no history).
- **`remind.nag`** — an RRULE that repeats **until the item is done** ("bother me every weekday at 10am"). Independent of the due date in both its rule and its expansion: `rdate` and `exdate` apply to the **due series only**. Fixed 2026-08-13 — `Schedule.expand` used to read those two sets off the schedule and apply them to whichever rule it expanded, nag included, so an item due 2026-08-10 with `nag FREQ=DAILY;COUNT=2` and `rdate ["2026-12-25T09:00-08:00"]` nagged three times, the third straight off the due schedule. They are now parameters passed per series and `NagOccurrences` passes neither, pinned by `TestDueDateOverridesDoNotMoveTheNag`. ⚠️ `reminder-coordinator` links noteboard's `model` package through a `replace` directive, so **the live reminders only change when that binary is rebuilt** — check it was redeployed before trusting this paragraph.
- **`remind.lead`** — ISO-8601 offsets before due (`-P1D`), calendar-aware.
- **`remind.channels`** — defaults to `["digest"]`. `calendar` and `herald` are opt-in per item.

A bad rule is rejected at write time — it would otherwise expand to nothing and silently remind no one. Always check `/occurrences` before trusting a rule; `BYDAY=3TU` and `BYDAY=TU;BYSETPOS=3` read alike and diverge in months that start on a Tuesday.

# Access and operations

## Who may call it

Canonical store for personal notes, todos, and ranked lists. Agents should read and write directly to the noteboard REST API on `localhost:8191`. Dash also proxies it at `/api/noteboard/*` for the browser UI, and *that* surface is behind dash's auth. ⚠️ noteboard itself has no auth at all, and it is not on the local socket — it listens on `*:8191` with no firewall rule, so anything that can route to this host has full read and write. Systemd user unit: `noteboard.service`. The two variables it reads, `NOTEBOARD_PORT` and `NOTEBOARD_DB`, are declared once in `internal/settings` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`, which makes `deploy-gate` check that tree too) and served read-only at `GET /settings`, as open as every other route, so declare nothing `Editable` and no secret there. A new variable is declared there or `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails. ⚠️ noteboard owns the prefixes `NOTEBOARD_PORT` and `NOTEBOARD_DB`, **not** `NOTEBOARD_`: a misspelling of either stops the service at boot, and `NOTEBOARD_URL`, which other programs read to find noteboard, never does. `deploy.sh` checks the running service's environment before it installs.

# Working in this repo

## Generated TypeScript types

Its wire types are rendered to TypeScript by `./generate-ts.sh` (tygo over `model/`) as `@kayushkin/noteboard-types` (`ts/model.ts`, `file:../noteboard/ts`, since 2026-09-17); kanban-store's generated types import `Item` from it, and bridge-ui takes `NoteboardItem` from it instead of a hand copy.
