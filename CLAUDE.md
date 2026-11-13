# About time-ledger-store

## What it owns

`127.0.0.1:8344` by default (`TIME_LEDGER_STORE_ADDR`), loopback only, no auth: put a front door in front. An account of what the operator did: **time entries** (`time_entry_000001`), each one thing done, or planned and skipped, over a span of time; **kinds** (`time_entry_kind_000001`), labels an entry carries any number of; and **reviews** (`time_entry_review_000001`), judgments of whether an entry happened. All three ids are reference-store types, so they draw as chips. `CONTRACT.md` is the route table. What the operator did is private: never copy an entry, a reason or a review into noteboard or a repo.

It owns no record of another store. Everything an entry rests on is a **link** by the owning store's id: calendar-store events, discord-signup-store events and attendance records, event-store suggestions, message-store messages, location-store trips and visits, photo-store photos, budget-store transactions and activity-store activities. This host's facts (port, jobs, first import) are in the noteboard note tagged `time-ledger-store`.

# How it works

## Entries nest and overlap

Entries overlap freely. An entry may sit inside one parent (`parent_time_entry_id`): a week away holds a day at a building, which holds an hour of work there. A child must lie inside its parent's span (409 otherwise), a span change that would leave a child outside is refused, and an entry cannot sit inside its own descendant. Deleting an entry is soft, refused while it has live children, and leaves its links in place, so an importer finds its source taken and never makes it again.

## Links, one table per kind of record

Each kind of linked record has its own table (`time_entry_calendar_events`, `time_entry_visits`…), with columns of its own: which date of a series (`occurrence_starts_at`, the date's original start) for calendar and Discord events, `overlap_seconds` for trips and visits, `amount_cents` for transactions. `LinkTypes` in `links.go` is the one definition the link SQL is built from. A link's `bears_on` says what it is to the entry: `source` (brought it in), `done`, `skipped`, `cost` or `context`. A live source belongs to one entry only, which is how one calendar date maps to one entry.

## Who decided, and who may change it

Every write names `by` and `by_kind`: `program` < `model` < `operator`. A title, span, place, parent, kind or link set by one kind is changed only by the same kind or higher; a kind or link the operator took off is never put back by a model or a program. Reviews are the operator's or a model's: the newest operator review gives the outcome, else the newest model review, else `unknown`. An entry needs review when no review is newer than `evidence_changed_at`, which moves when its span or a link changes.

## The three commands

`time-ledger-import` makes entries from journeys (location-store trips of kind journey, kind `journey`), every date of every calendar event in a window (series expanded by `internal/occurrences`, less EXDATEs and dates a changed copy stands in for; kind `routine`), the operator's Discord attendance records (attendance linked as `done`), and event-store suggestions added to the calendar (a second source, matched through calendar-store's `source_event_id`); then puts each entry inside the shortest journey that holds it. An all-day date with no zone is read in `-time-zone`. `time-ledger-gather-evidence` links overlapping visits and photos taken during each ended entry of at most a day, as `context`, then copies budget-store's spending on each trip to the entry that trip is the source of: a confirmed charge as `cost`, a suggested one as `context`, and a removed one taken off again if the command linked it. `time-ledger-review` sends each entry needing review, with every linked record read whole from its store, to the model of model-store role `time-entry-reviewer` through llm-bridge-server's `POST /oneshot`, keeps the answer as a model review, and applies the kinds and parent it names as the model's. Run them in that order.

# Access and operations

## Deploy and settings

`./deploy.sh` runs the shared deploy gate and the tests, builds the service and the three commands, checks the running unit's environment against the declared settings, installs to `~/bin`, restarts the user unit `time-ledger-store`, and fails unless `/health` is ok and the port is bound to loopback. Every variable the service reads is declared in `settings.go` with llm-bridge `servicesettings` and served at `GET /settings`; `time-ledger-review`'s (`LLMBRIDGE_URL`, `LLMBRIDGE_SERVICE_TOKEN`, `MODEL_STORE_URL`) are declared there too. The reviewer's role must exist in model-store and in the llm-bridge-server build, which compiles model-store's role list in.

# Working in this repo

## Tests

`go test ./...` covers the store's rules (one entry per source date, children inside parents, precedence, outcome, review freshness, link checks) and series expansion. Try a command against live data on a throwaway instance first: start the service with `TIME_LEDGER_STORE_ADDR` on a spare port and `TIME_LEDGER_STORE_DATA_DIR` in `/tmp`, point the command's `-time-ledger-store-url` at it, and use `time-ledger-review -dry-run -entry <id>` to read a prompt without calling a model.
