# eval-store — API contract

Port 8323, set by `EVAL_STORE_ADDR` in `eval-store.service` (loopback only). Judged
runs of several models on a fixed set of real model calls.

Module `github.com/kayushkin/eval-store`, root package `evalstore`, binary
`cmd/eval-store` installed at `~/bin/eval-store`. SQLite at
`<EVAL_STORE_DATA_DIR>/eval-store.db` (default `~/.config/eval-store`), WAL, foreign
keys on. The directory is 700 and the file 600, and `Open` refuses a directory
others can read: **cases hold real mail**. Nothing derived from a case — prompt,
answer, verdict reason — may go in this repository, its tests or its commit
messages.

Routes are rooted at `/`. No route takes a credential. Times are epoch seconds
(0 = unset). Bodies are decoded strictly: an unknown field is a 400. Errors are
`{"error": "…"}` with 400 (invalid), 404, 409 (conflict), 502 (llm-bridge-server
lacks a route) or 500.

## Records and ids

| Record | Id | What it is |
|---|---|---|
| suite | `eval_suite_000001` | a task and how to judge it |
| case | `eval_case_000001` | one real model call, frozen |
| run | `eval_run_000001` | candidates, judges, cases and frozen prices |
| answer | `eval_answer_000001` | one candidate's answer to one case in one run |
| verdict | `eval_verdict_000001` | one judge's verdict on every candidate of one case |
| check | (run, case, model, check name) | one deterministic check of one answer |

Other stores' records are named by their ids only: model-store model ids,
principal-store `principal_…` ids, llm-bridge-server `operation_…` ids, and a
run's optional `prediction_id` (`prediction_…`, owned by prediction-store).

### Suite

| Field | Meaning |
|---|---|
| `name`, `description` | display only; never used to find a suite |
| `task_kind` | e.g. `email-filing`, `demo-mail` |
| `judge_instructions` | shown to every judge ahead of the task |
| `judge_failure_kinds` | the kinds a judge may name; must hold `none` and `no_answer` |
| `output_schema` | the JSON Schema every candidate's answer is forced to |
| `max_tokens`, `judge_max_tokens` | output limits for candidates and judges |
| `checker_name` | a registered checker (`GET /vocabulary`), or empty |
| `baseline_model_id` | the model reports compare others with |
| `weekly_calls_source` | `{"kind":"fixed","calls_per_week":N,"note":"where N came from"}` or `{"kind":"llm-bridge-operations","operations":{filter}}` (counts the last seven days when a report asks) |
| `cost_model` | see *Cost projection* |

### Case

`source_kind` (`llm-bridge-operation` or `claude-code-transcript`) and
`source_ref` (the owning store's id) say where it came from; a suite holds one
case per source. The prompts, the original answer, its usage
(`original_input_tokens`, `original_cache_read_tokens`,
`original_cache_write_tokens`, `original_output_tokens`, `original_usage_known`),
`original_duration_ms` and `original_model_id` are a **snapshot**, because the
sources may be pruned. `features` are numbers the suite's checker computed at
import (`cards`, `emails` for filing; `messages` for demo mail; `expected_questions`
for turn-end signals). `expected_answer` is optional JSON, written by whoever adds the
case, saying what a right answer holds in the shape the suite's checker reads; a checker
that reads it refuses an unreadable one when the case is added (400), a suite whose checker
reads none refuses any, and only `GET /cases/{id}` returns it.

### Run

`candidate_model_ids` (2 to 26), `judge_model_ids`, `original_answer_model_id`
(a candidate whose answers are the cases' originals rather than replays),
`case_selection` (`{"mode":"all"}` or `{"mode":"sample","size":N,"seed":S}`,
frozen into `case_ids` at creation), `budget_limits` (`[{"subscription":"codex",
"window":"weekly","max_used_percent":60}, …]`; defaults from
`EVAL_STORE_DEFAULT_BUDGET_LIMITS`), `concurrency`, `organization_id` (the
principal-store group billed, recorded at start; default
`EVAL_STORE_ORGANIZATION_ID`; a run with neither refuses to start), `model_prices` (every model's provider and
list price from model-store, frozen at creation), `prediction_id`, `note`,
`origin` (`live` or `legacy-import`).

Statuses: `created` → `running` → `completed`, or `stopping` → `stopped` (asked),
`stopped_budget` (usage past a limit, a stale or missing usage reading, or a
budget refusal from the bridge), `interrupted` (the service stopped), `failed`.
Every one but `running` and `stopping` can be resumed.

### Answer, verdict, check

An answer has `origin` (`replayed`, `case-original`, `legacy-import`),
`attempt`, `parsed`, `error`, the four token counts, `duration_ms`,
`operation_id` and `answered_by` (the model the bridge says answered; a
different one from the one asked is recorded as an error).

A verdict has `label_mapping` (label → model: the blind shuffle, seeded by the
case id alone, so every judge of a case sees one order), `parsed`, `valid`,
`invalid_reason`, `error`, `judge_provider`, tokens, `operation_id`, and when
valid its `candidates` lines (model, label, rank, acceptable, failure_kind,
reason). A verdict is valid when its ranking and its verdicts each name every
label exactly once; an invalid one is left out of every report figure.

## Model calls

Every candidate and judge call is an `llm.completion` operation on
llm-bridge-server, sent with `X-LLM-Bridge-Service-Token` alone, as scheduler
sends its calls: eval-store acts as the internal service, as no principal, and
each intent bills the run's `organization_id`. The operation's
`model` is the model-store id. Idempotency key:
`eval-store:<run>:<case>:<answer|judge>:<model>:<attempt>`; correlation id: the
run id. A resumed run sends the same keys and is handed the first receipts.
Calls are made in batches of `EVAL_STORE_BUDGET_CHECK_EVERY_CALLS`, with
usage-store read before each.

## Routes

| Route | What it does |
|---|---|
| `GET /health` | counts of each record |
| `GET /settings` | the declared settings and the values in force; the token as set-or-not |
| `GET /vocabulary` | run statuses, source kinds, checkers, selection modes, answer origins |
| `GET /suites` · `POST /suites` | list, create |
| `GET /suites/{id}` · `PATCH /suites/{id}` · `DELETE /suites/{id}` | read; change the fields named; delete (409 while it has runs) |
| `GET /suites/{id}/cases` | the suite's cases **without** prompts or answers |
| `POST /suites/{id}/cases` | add one case |
| `POST /suites/{id}/cases/import-operations` | body: `type` (only `llm.completion`), `organization_id`, `principal_id`, `correlation_id_prefix`, `created_after`, `created_before` (RFC 3339), `sample_size`, `seed`. Imports succeeded operations not yet in the suite. Answers `matched`, `already_imported`, `imported` (case ids) and `schema_differs` (operations whose schema is not the suite's). **Needs `GET /operations/{id}/intent` on llm-bridge-server, which does not exist yet: 502 until it does.** |
| `GET /cases/{id}` | one case **with** its prompts and original answer |
| `PATCH /cases/{id}` | `{"expected_answer": …}`, the only field that changes: replaces the expectation (held to the suite's checker, 400 otherwise) and recomputes `features`. Stored checks are left as they were; `POST /runs/{id}/checks` runs the checker again |
| `GET /runs?suite_id=` · `POST /runs` | list (newest first); create (prices read from model-store; an unknown model is a 400) |
| `GET /runs/{id}` | the run and its `progress` |
| `PATCH /runs/{id}` | `prediction_id`, `note` |
| `DELETE /runs/{id}` | with its answers, verdicts and checks (409 while going) |
| `POST /runs/{id}/start` | start a `created` run (202) |
| `POST /runs/{id}/resume` | `{"retry_failed": false}`: make only what is missing; with `true` also what failed, under the next attempt's key. A budget refusal is retried either way |
| `POST /runs/{id}/stop` | finish the calls in flight, then `stopped` |
| `GET /runs/{id}/answers?case_id=&model_id=&include_bodies=` | answers; `parsed` only with `include_bodies=true` |
| `GET /runs/{id}/verdicts?case_id=&judge_model_id=&include_bodies=` | verdicts; `parsed` and reasons only with `include_bodies=true` |
| `GET /runs/{id}/checks` · `POST /runs/{id}/checks` | check results; run the checker again |
| `GET /runs/{id}/report?format=json\|markdown&calls_per_week=` | see below |
| `GET /runs/{id}/cost-projection?…` | see below |

## Report

Per candidate: per judge, acceptable/judged and mean rank; on the cases every
judge judged validly, how many all judges accept and how many they agree on;
failure kinds counted per (judge, kind, acceptable) for every verdict that is
unacceptable or names a kind other than `none`; token means (input includes
cache reads and writes), output median; cost per call at list price and
cache-adjusted (**an assumption**: cache reads at 10% of the input price,
Anthropic cache writes at 125%, others 100%); cost per week (calls per week
from the request or the suite's source); latency median and p90 (the bench's
nearest rank, rounding half to even). Per run: each judge's valid, malformed
and failed counts, whether it is a candidate and which candidates share its
provider; how often every judge put the same answer first, and the winners
then; case-by-case against the baseline (all judges prefer the model, all
prefer the baseline, split); check sums; and, when the checker compares
choices, how often each model chose what the baseline chose, unit by unit.

## Cost projection

`target_usd` (default 100), `calls_per_week` (a task that does not batch;
default the suite's source), `weekly.<feature>` (a task that batches: units a
week, required), `feature.<name>` (a per-call value; default the mean over the
run's cases). The suite's `cost_model`:

```json
{"features": ["cards", "emails"], "fixed_input_tokens": 3800,
 "input_tokens_per_feature": {"cards": 70, "emails": 700},
 "output_per_feature": "emails",
 "batching": {"feature": "emails", "ticks_per_week": 672, "minimum_per_call": 1.4, "maximum_per_call": 10},
 "note": "…"}
```

Input per call = harness multiplier × (fixed + Σ coefficient × feature). A
coefficient left out is fitted by least squares to the baseline's answers. The
multiplier is a model's mean input over the baseline's. Output is per unit of
`output_per_feature`, or per call. A batching task makes `V / per_call` calls a
week, `per_call = clamp(V / ticks_per_week, minimum, maximum)`. List prices, no
cache discount. Each model gets cost per call, week and unit, and the volume,
each feature and each coefficient that reach `target_usd` (null when out of
reach below 10⁷).

## Command

`eval-store serve`; `eval-store import-legacy <dir>` (opens the database
directly); every other verb calls the server at `EVAL_STORE_URL`. `eval-store
help` lists them.
