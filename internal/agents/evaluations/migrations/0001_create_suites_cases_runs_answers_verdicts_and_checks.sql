-- eval-store: judged runs of several models on a fixed set of real model calls.
--
-- Every id is a prefix and the row's sequence number, at least six digits
-- (eval_suite_000001), written by postgresstorage.FormatRecordID when the row
-- is inserted. Times are epoch seconds, 0 = unset. JSON columns hold the JSON
-- text exactly as it was written. Flags are 0 or 1.
--
-- Cases hold real prompts, and for email filing those are real personal mail.
-- Nothing in this file or the repository may carry case content.

CREATE TABLE suites (
  seq                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id                   text NOT NULL UNIQUE,
  name                 text NOT NULL,
  description          text NOT NULL DEFAULT '',
  -- What the task is, e.g. email-filing or demo-mail. Free text.
  task_kind            text NOT NULL,
  -- Shown to every judge ahead of the task, saying what counts as a failure.
  judge_instructions   text NOT NULL,
  -- JSON array of the failure kinds a judge may name; holds none and no_answer.
  judge_failure_kinds  text NOT NULL,
  -- JSON Schema every answer to this task follows. Candidates are forced to it.
  output_schema        text NOT NULL,
  max_tokens           bigint NOT NULL,
  judge_max_tokens     bigint NOT NULL,
  -- A checker registered in checker.go, or empty for none.
  checker_name         text NOT NULL DEFAULT '',
  -- A model-store id that reports compare every other model with. Optional.
  baseline_model_id    text NOT NULL DEFAULT '',
  -- JSON: where calls per week come from (see WeeklyCallsSource).
  weekly_calls_source  text NOT NULL DEFAULT '{}',
  -- JSON: how GET /runs/{id}/cost-projection models a week (see CostModel).
  cost_model           text NOT NULL DEFAULT '{}',
  created_at           bigint NOT NULL,
  updated_at           bigint NOT NULL
);

-- A case is one real model call, frozen. The prompts and the original answer
-- are a deliberate snapshot rather than a reference: llm-bridge-server
-- operations and Claude Code transcripts may be pruned, and a judged run must
-- stay readable after its sources are gone. source_kind and source_ref still
-- name the record it came from, by the owning store's id.
CREATE TABLE cases (
  seq                          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id                           text NOT NULL UNIQUE,
  suite_id                     text NOT NULL REFERENCES suites(id) ON DELETE CASCADE,
  -- llm-bridge-operation or claude-code-transcript.
  source_kind                  text NOT NULL,
  -- operation_<ULID> for an operation; the transcript's session id otherwise.
  source_ref                   text NOT NULL,
  system_prompt                text NOT NULL,
  user_prompt                  text NOT NULL,
  -- The model that gave the original answer, a model-store id, when known.
  original_model_id            text NOT NULL DEFAULT '',
  -- The original answer as JSON, or NULL when the source had none.
  original_answer              text,
  original_usage_known         bigint NOT NULL DEFAULT 0 CHECK (original_usage_known IN (0, 1)),
  original_input_tokens        bigint NOT NULL DEFAULT 0,
  original_cache_read_tokens   bigint NOT NULL DEFAULT 0,
  original_cache_write_tokens  bigint NOT NULL DEFAULT 0,
  original_output_tokens       bigint NOT NULL DEFAULT 0,
  -- NULL when the source did not say how long the call took.
  original_duration_ms         bigint,
  -- When the original call was made.
  original_at                  bigint NOT NULL DEFAULT 0,
  -- JSON object of numbers the suite's checker computed at import.
  features                     text NOT NULL DEFAULT '{}',
  created_at                   bigint NOT NULL,
  -- What a right answer must hold, as JSON in the shape the suite's checker
  -- reads, written by whoever added the case; NULL when nobody wrote one.
  expected_answer              text,
  CONSTRAINT cases_one_per_source UNIQUE (suite_id, source_kind, source_ref)
);
CREATE INDEX cases_suite ON cases(suite_id);

CREATE TABLE runs (
  seq                       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id                        text NOT NULL UNIQUE,
  suite_id                  text NOT NULL REFERENCES suites(id),
  -- JSON arrays of model-store ids.
  candidate_model_ids       text NOT NULL,
  judge_model_ids           text NOT NULL,
  -- A candidate whose answers are the cases' original answers, not replays.
  original_answer_model_id  text NOT NULL DEFAULT '',
  -- JSON: {"mode":"all"} or {"mode":"sample","size":N,"seed":S}.
  case_selection            text NOT NULL,
  -- JSON array of case ids, frozen when the run is created.
  case_ids                  text NOT NULL,
  status                    text NOT NULL,
  status_reason             text NOT NULL DEFAULT '',
  -- JSON array of {subscription, window, max_used_percent}.
  budget_limits             text NOT NULL,
  concurrency               bigint NOT NULL,
  -- The principal-store group the calls are billed to, as it was when the
  -- run last started.
  organization_id           text NOT NULL DEFAULT '',
  -- JSON object model id -> {name, provider, input_cost, output_cost}, read
  -- from model-store when the run was created and never changed after.
  model_prices              text NOT NULL,
  prices_note               text NOT NULL DEFAULT '',
  -- A prediction-store id this run bears on. prediction-store owns it.
  prediction_id             text NOT NULL DEFAULT '',
  note                      text NOT NULL DEFAULT '',
  -- live, or legacy-import for runs rebuilt from the Python bench.
  origin                    text NOT NULL,
  created_at                bigint NOT NULL,
  started_at                bigint NOT NULL DEFAULT 0,
  finished_at               bigint NOT NULL DEFAULT 0,
  updated_at                bigint NOT NULL
);

-- One model's answer to one case in one run.
CREATE TABLE answers (
  seq                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id                  text NOT NULL UNIQUE,
  run_id              text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  case_id             text NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  model_id            text NOT NULL,
  -- replayed, case-original, copied (from another run) or legacy-import.
  origin              text NOT NULL,
  -- Part of the idempotency key: a retry after a failure needs a new key.
  attempt             bigint NOT NULL DEFAULT 1,
  parsed              text,
  error               text NOT NULL DEFAULT '',
  input_tokens        bigint NOT NULL DEFAULT 0,
  cache_read_tokens   bigint NOT NULL DEFAULT 0,
  cache_write_tokens  bigint NOT NULL DEFAULT 0,
  output_tokens       bigint NOT NULL DEFAULT 0,
  usage_known         bigint NOT NULL DEFAULT 0 CHECK (usage_known IN (0, 1)),
  duration_ms         bigint,
  operation_id        text NOT NULL DEFAULT '',
  -- The model the bridge says answered.
  answered_by         text NOT NULL DEFAULT '',
  created_at          bigint NOT NULL,
  UNIQUE (run_id, case_id, model_id)
);

-- One judge's verdict on all candidates of one case in one run.
CREATE TABLE verdicts (
  seq              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id               text NOT NULL UNIQUE,
  run_id           text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  case_id          text NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  judge_model_id   text NOT NULL,
  -- The judge's provider, from model-store, for the self-judging flag.
  judge_provider   text NOT NULL DEFAULT '',
  attempt          bigint NOT NULL DEFAULT 1,
  -- JSON object label -> model id: the blind shuffle the judge saw.
  label_mapping    text NOT NULL,
  parsed           text,
  -- 1 when parsed ranks and judges every label exactly once.
  valid            bigint NOT NULL DEFAULT 0 CHECK (valid IN (0, 1)),
  invalid_reason   text NOT NULL DEFAULT '',
  error            text NOT NULL DEFAULT '',
  input_tokens     bigint NOT NULL DEFAULT 0,
  output_tokens    bigint NOT NULL DEFAULT 0,
  duration_ms      bigint,
  operation_id     text NOT NULL DEFAULT '',
  created_at       bigint NOT NULL,
  UNIQUE (run_id, case_id, judge_model_id)
);

-- The per-candidate lines of a valid verdict, unshuffled.
CREATE TABLE verdict_candidates (
  verdict_id    text NOT NULL REFERENCES verdicts(id) ON DELETE CASCADE,
  model_id      text NOT NULL,
  label         text NOT NULL,
  rank          bigint NOT NULL,
  acceptable    bigint NOT NULL CHECK (acceptable IN (0, 1)),
  failure_kind  text NOT NULL,
  reason        text NOT NULL,
  PRIMARY KEY (verdict_id, model_id)
);

-- Deterministic checks of one answer, by name.
CREATE TABLE checks (
  run_id      text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  case_id     text NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
  model_id    text NOT NULL,
  check_name  text NOT NULL,
  passed      bigint NOT NULL CHECK (passed IN (0, 1)),
  -- A tally the check counts: offending items, or for a tally check the count.
  count       bigint NOT NULL DEFAULT 0,
  detail      text NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, case_id, model_id, check_name)
);
