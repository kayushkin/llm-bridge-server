-- eval-store: judged runs of several models on a fixed set of real model calls.
--
-- Every id is a prefix and a six-digit sequence (eval_suite_000001), assigned
-- here. Times are epoch seconds, 0 = unset. JSON columns hold JSON text.
--
-- Cases hold real prompts, and for email filing those are real personal mail.
-- The database lives outside the repository, in a directory of mode 700, and
-- the file is mode 600. Nothing in this file or the repository may carry case
-- content.

CREATE TABLE IF NOT EXISTS suites (
  seq                  INTEGER PRIMARY KEY AUTOINCREMENT,
  id                   TEXT NOT NULL UNIQUE,
  name                 TEXT NOT NULL,
  description          TEXT NOT NULL DEFAULT '',
  -- What the task is, e.g. email-filing or demo-mail. Free text.
  task_kind            TEXT NOT NULL,
  -- Shown to every judge ahead of the task, saying what counts as a failure.
  judge_instructions   TEXT NOT NULL,
  -- JSON array of the failure kinds a judge may name; holds none and no_answer.
  judge_failure_kinds  TEXT NOT NULL,
  -- JSON Schema every answer to this task follows. Candidates are forced to it.
  output_schema        TEXT NOT NULL,
  max_tokens           INTEGER NOT NULL,
  judge_max_tokens     INTEGER NOT NULL,
  -- A checker registered in checkers.go, or empty for none.
  checker_name         TEXT NOT NULL DEFAULT '',
  -- A model-store id that reports compare every other model with. Optional.
  baseline_model_id    TEXT NOT NULL DEFAULT '',
  -- JSON: where calls per week come from (see WeeklyCallsSource).
  weekly_calls_source  TEXT NOT NULL DEFAULT '{}',
  -- JSON: how GET /runs/{id}/cost-projection models a week (see CostModel).
  cost_model           TEXT NOT NULL DEFAULT '{}',
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);

-- A case is one real model call, frozen. The prompts and the original answer
-- are a deliberate snapshot rather than a reference: llm-bridge-server
-- operations and Claude Code transcripts may be pruned, and a judged run must
-- stay readable after its sources are gone. source_kind and source_ref still
-- name the record it came from, by the owning store's id.
CREATE TABLE IF NOT EXISTS cases (
  seq                          INTEGER PRIMARY KEY AUTOINCREMENT,
  id                           TEXT NOT NULL UNIQUE,
  suite_id                     TEXT NOT NULL REFERENCES suites(id) ON DELETE CASCADE,
  -- llm-bridge-operation or claude-code-transcript.
  source_kind                  TEXT NOT NULL,
  -- operation_<ULID> for an operation; the transcript's session id otherwise.
  source_ref                   TEXT NOT NULL,
  system_prompt                TEXT NOT NULL,
  user_prompt                  TEXT NOT NULL,
  -- The model that gave the original answer, a model-store id, when known.
  original_model_id            TEXT NOT NULL DEFAULT '',
  -- The original answer as JSON, or NULL when the source had none.
  original_answer              TEXT,
  original_usage_known         INTEGER NOT NULL DEFAULT 0,
  original_input_tokens        INTEGER NOT NULL DEFAULT 0,
  original_cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  original_cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
  original_output_tokens       INTEGER NOT NULL DEFAULT 0,
  -- NULL when the source did not say how long the call took.
  original_duration_ms         INTEGER,
  -- When the original call was made.
  original_at                  INTEGER NOT NULL DEFAULT 0,
  -- JSON object of numbers the suite's checker computed at import.
  features                     TEXT NOT NULL DEFAULT '{}',
  created_at                   INTEGER NOT NULL,
  -- What a right answer must hold, as JSON in the shape the suite's checker
  -- reads, written by whoever added the case; NULL when nobody wrote one.
  -- addMissingColumns adds it to a database made before it existed.
  expected_answer              TEXT,
  UNIQUE (suite_id, source_kind, source_ref)
);
CREATE INDEX IF NOT EXISTS cases_suite ON cases(suite_id);

CREATE TABLE IF NOT EXISTS runs (
  seq                       INTEGER PRIMARY KEY AUTOINCREMENT,
  id                        TEXT NOT NULL UNIQUE,
  suite_id                  TEXT NOT NULL REFERENCES suites(id),
  -- JSON arrays of model-store ids.
  candidate_model_ids       TEXT NOT NULL,
  judge_model_ids           TEXT NOT NULL,
  -- A candidate whose answers are the cases' original answers, not replays.
  original_answer_model_id  TEXT NOT NULL DEFAULT '',
  -- JSON: {"mode":"all"} or {"mode":"sample","size":N,"seed":S}.
  case_selection            TEXT NOT NULL,
  -- JSON array of case ids, frozen when the run is created.
  case_ids                  TEXT NOT NULL,
  status                    TEXT NOT NULL,
  status_reason             TEXT NOT NULL DEFAULT '',
  -- JSON array of {subscription, window, max_used_percent}.
  budget_limits             TEXT NOT NULL,
  concurrency               INTEGER NOT NULL,
  -- The principal-store group the calls are billed to, as it was when the
  -- run last started.
  organization_id           TEXT NOT NULL DEFAULT '',
  -- JSON object model id -> {name, provider, input_cost, output_cost}, read
  -- from model-store when the run was created and never changed after.
  model_prices              TEXT NOT NULL,
  prices_note               TEXT NOT NULL DEFAULT '',
  -- A prediction-store id this run bears on. prediction-store owns it.
  prediction_id             TEXT NOT NULL DEFAULT '',
  note                      TEXT NOT NULL DEFAULT '',
  -- live, or legacy-import for runs rebuilt from the Python bench.
  origin                    TEXT NOT NULL,
  created_at                INTEGER NOT NULL,
  started_at                INTEGER NOT NULL DEFAULT 0,
  finished_at               INTEGER NOT NULL DEFAULT 0,
  updated_at                INTEGER NOT NULL
);

-- One model's answer to one case in one run.
CREATE TABLE IF NOT EXISTS answers (
  seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
  id                  TEXT NOT NULL UNIQUE,
  run_id              TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  case_id             TEXT NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  model_id            TEXT NOT NULL,
  -- replayed, case-original, copied (from another run) or legacy-import.
  origin              TEXT NOT NULL,
  -- Part of the idempotency key: a retry after a failure needs a new key.
  attempt             INTEGER NOT NULL DEFAULT 1,
  parsed              TEXT,
  error               TEXT NOT NULL DEFAULT '',
  input_tokens        INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens       INTEGER NOT NULL DEFAULT 0,
  usage_known         INTEGER NOT NULL DEFAULT 0,
  duration_ms         INTEGER,
  operation_id        TEXT NOT NULL DEFAULT '',
  -- The model the bridge says answered.
  answered_by         TEXT NOT NULL DEFAULT '',
  created_at          INTEGER NOT NULL,
  UNIQUE (run_id, case_id, model_id)
);

-- One judge's verdict on all candidates of one case in one run.
CREATE TABLE IF NOT EXISTS verdicts (
  seq              INTEGER PRIMARY KEY AUTOINCREMENT,
  id               TEXT NOT NULL UNIQUE,
  run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  case_id          TEXT NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  judge_model_id   TEXT NOT NULL,
  -- The judge's provider, from model-store, for the self-judging flag.
  judge_provider   TEXT NOT NULL DEFAULT '',
  attempt          INTEGER NOT NULL DEFAULT 1,
  -- JSON object label -> model id: the blind shuffle the judge saw.
  label_mapping    TEXT NOT NULL,
  parsed           TEXT,
  -- 1 when parsed ranks and judges every label exactly once.
  valid            INTEGER NOT NULL DEFAULT 0,
  invalid_reason   TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  input_tokens     INTEGER NOT NULL DEFAULT 0,
  output_tokens    INTEGER NOT NULL DEFAULT 0,
  duration_ms      INTEGER,
  operation_id     TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  UNIQUE (run_id, case_id, judge_model_id)
);

-- The per-candidate lines of a valid verdict, unshuffled.
CREATE TABLE IF NOT EXISTS verdict_candidates (
  verdict_id    TEXT NOT NULL REFERENCES verdicts(id) ON DELETE CASCADE,
  model_id      TEXT NOT NULL,
  label         TEXT NOT NULL,
  rank          INTEGER NOT NULL,
  acceptable    INTEGER NOT NULL,
  failure_kind  TEXT NOT NULL,
  reason        TEXT NOT NULL,
  PRIMARY KEY (verdict_id, model_id)
);

-- Deterministic checks of one answer, by name.
CREATE TABLE IF NOT EXISTS checks (
  run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  case_id     TEXT NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  model_id    TEXT NOT NULL,
  check_name  TEXT NOT NULL,
  passed      INTEGER NOT NULL,
  -- A tally the check counts: offending items, or for a tally check the count.
  count       INTEGER NOT NULL DEFAULT 0,
  detail      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, case_id, model_id, check_name)
);
