# About prediction-store

## What it owns

`:8313`. The calibration ledger: resolvable claims, the odds stated for them, and what actually happened. It exists to make one rule of the host prompt measurable — "start from the base rate" had no base rate to start from, so every such number was invented; `GET /base-rates?group_by=category` answers it from resolved rows. Routes are rooted at `/`, not `/api`. `CONTRACT.md` is the route table.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Three tables

`predictions` holds the claim, its **required** resolution criteria, category, provenance, due date and outcome. `estimates` is **append-only**: every revision of the number with the evidence that moved it, so the opening odds survive an update; it has no update route and no delete route. `links` is `entity_type` + `entity_ref` pointing at another store's record — ids, never names.

## The number and the outcome each move through one route

**The probability moves only through `POST /predictions/{id}/estimates`, and the outcome only through `POST /predictions/{id}/resolve`, which works once** — a second call is 409. `PATCH /predictions/{id}` decodes strictly and its `Patch` type deliberately has no `probability`, `outcome` or `status` field, so PATCHing one is a 400 that names the right route rather than a silent no-op. Do not add those fields to `Patch`: a caller who could quietly revise a number to match the outcome would make the whole ledger worthless. A claim that stopped being answerable resolves `void` and is left out of scoring rather than counted as a miss.

## The probability ladder and the vocabulary

A probability must sit on a fixed ladder (0.001, 0.01, 0.05 … 0.95, 0.99, 0.999; `ProbabilityLadder` in `vocabulary.go`); `0.73` is a **400**, because it claims a precision nobody has and it blurs the buckets. Categories, provenances, statuses, outcomes and the `group_by` keys are lists in `vocabulary.go`, served at `GET /vocabulary`; an unknown category is refused with the whole list in the message. Callers and UIs read the vocabulary from the route and never restate it.

## Ids are prefixed

⚠️ **Ids are `prediction_000001`, never a bare uuid.** dash's resolver probes every entity-type row whose id pattern matches, and noteboard already claims the uuid shape — uuid ids here would make every uuid in every chat message probe this store too. A link's own id is a plain integer: `DELETE /links/{id}` takes that, not the prediction id.

## Calibration scoring

`GET /calibration` reports the Brier score, the log score, the overconfidence gap (mean confidence against accuracy, which reads a correct 20% call as correct where mean-against-observed does not) and Murphy's decomposition into reliability and resolution — exact, because the ladder quantises the probabilities. It slices by `category`, `provenance`, `author`, `tag`, `entity_type`, `lead_time` or `probability`. Only resolved, non-void rows are scored.

## Linking a prediction to other records

`POST /predictions/{id}/links` takes `entity_type` and `entity_ref`; both are required, and `entity_type` is free text — the store does not check it against reference-store's list of reference types (its `GET /types` is the canonical list). **kanban-store needs no change to link a card to a prediction**: it does not validate `entity_type` either, so `POST /api/cards/{id}/links {"entity_type":"prediction",…}` works as `pull_request` does. Registering the type would only buy chat chips and costs three edits in three repos.

## Deleting

`DELETE /predictions/{id}` is a soft delete: the row leaves every read path except `?include_deleted`, stays readable by id, and `POST /predictions/{id}/restore` brings it back. `?hard=true` purges it and takes its estimates and links by cascade.

# Access and operations

## Who may call it, and what runs against it

No authentication. The unit sets `PREDICTION_STORE_ADDR=:8313`, so it listens on every interface. dash proxies it unchanged at `/api/predictions/*` (`dash/server/api_predictions.go`, target from `PREDICTION_STORE_URL`). Unit `prediction-store.service`, binary `~/bin/prediction-store`. Scheduler job 76, `prediction-resolve-sweep` (daily 09:00, `scripts/prediction-resolve-sweep.sh`), keeps **one** noteboard todo listing the open predictions whose due date has passed. It reminds nobody itself: `reminder-coordinator` delivers that todo through the todo's own `schedule.remind.nag`.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/prediction-store`, root package `predictionstore`, server in `cmd/prediction-store`. SQLite at `~/.config/prediction-store/prediction-store.db` (WAL, foreign keys on). The two variables it reads, `PREDICTION_STORE_ADDR` and `PREDICTION_STORE_DATA_DIR`, are declared once in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`, which makes `deploy-gate` check that tree too) and served read-only at `GET /settings`. A new variable is declared there or `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails, and a set `PREDICTION_STORE_` variable that nobody declared stops the service at boot; `deploy.sh` checks the running service's environment for one before it stops it. Declare nothing `Editable` while the service has no operator gate. ⚠️ **Build and test with `-tags sqlite_fts5`** — not optional. `deploy.sh` sets the tag, runs the tests, and checks after the restart that full-text search answers. Pushes to `github.com/kayushkin/prediction-store` (public).
