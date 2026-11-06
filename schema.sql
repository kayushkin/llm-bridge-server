-- budget-store schema. Create-only, so it is safe to run on every boot; a
-- change to an existing table needs a step in migrate() as well.
--
-- Double entry: a transaction holds two or more postings whose amounts sum to
-- zero. A posting lands on an account (money held or owed) or a category
-- (where money came from or went). Amounts are integer cents, USD only.
--
-- Sign: a posting on an account is the change in that account's balance. An
-- asset's balance is positive; a debt's balance is negative, so a card
-- purchase is a negative posting on the card and paying the card is positive.
-- A category posting is the other side: spending is positive on an expense
-- category, income is negative on an income category.
--
-- Every table has seq (the sequence) and id (prefix + seq zero-padded to six,
-- account_000001), so kanban-store's entity-type registry can match each kind
-- for chips. Times are epoch seconds (0 = unset); dates are 'YYYY-MM-DD'.

CREATE TABLE IF NOT EXISTS institutions (
  seq         INTEGER PRIMARY KEY,
  id          TEXT NOT NULL UNIQUE,            -- institution_000001
  name        TEXT NOT NULL,                   -- display only, never a join key
  website     TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- Where money sits or is owed. Facts that only one kind has live in the detail
-- tables below, one per kind.
CREATE TABLE IF NOT EXISTS accounts (
  seq             INTEGER PRIMARY KEY,
  id              TEXT NOT NULL UNIQUE,        -- account_000001
  institution_id  TEXT REFERENCES institutions(id),
  name            TEXT NOT NULL,
  kind            TEXT NOT NULL,               -- vocabulary.go: accountKinds
  last_four       TEXT NOT NULL DEFAULT '',    -- never the full number
  opened_on       TEXT NOT NULL DEFAULT '',
  closed_on       TEXT NOT NULL DEFAULT '',
  on_budget       INTEGER NOT NULL,            -- counts toward the envelope view's "ready to assign"
  has_own_feed    INTEGER NOT NULL,            -- 1: its rows arrive by import; 0: only other accounts' rows mention it (a loan with no export)
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

-- Text that, in another account's transaction description, names this
-- account: its last four, an old card number, a loan's reference. The
-- transfer matcher pairs two rows only when one names the other's account.
CREATE TABLE IF NOT EXISTS account_description_markers (
  seq         INTEGER PRIMARY KEY,
  id          TEXT NOT NULL UNIQUE,            -- account_description_marker_000001
  account_id  TEXT NOT NULL REFERENCES accounts(id),
  marker      TEXT NOT NULL,                   -- matched case-insensitively as a substring
  note        TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  UNIQUE (account_id, marker)
);

CREATE TABLE IF NOT EXISTS credit_card_details (
  account_id                TEXT PRIMARY KEY REFERENCES accounts(id),
  network                   TEXT NOT NULL DEFAULT '',
  credit_limit_cents        INTEGER NOT NULL DEFAULT 0,
  statement_day             INTEGER NOT NULL DEFAULT 0,  -- day of month, 0 = unknown
  due_day                   INTEGER NOT NULL DEFAULT 0,
  purchase_apr_basis_points INTEGER NOT NULL DEFAULT 0,  -- 2499 = 24.99%
  annual_fee_cents          INTEGER NOT NULL DEFAULT 0,
  rewards                   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS loan_details (
  account_id                TEXT PRIMARY KEY REFERENCES accounts(id),
  original_principal_cents  INTEGER NOT NULL DEFAULT 0,
  apr_basis_points          INTEGER NOT NULL DEFAULT 0,
  term_months               INTEGER NOT NULL DEFAULT 0,
  first_payment_on          TEXT NOT NULL DEFAULT '',
  maturity_on               TEXT NOT NULL DEFAULT '',
  scheduled_payment_cents   INTEGER NOT NULL DEFAULT 0,
  secured_by                TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS retirement_details (
  account_id     TEXT PRIMARY KEY REFERENCES accounts(id),
  plan_kind      TEXT NOT NULL,                -- vocabulary.go: retirementPlanKinds
  tax_treatment  TEXT NOT NULL,                -- vocabulary.go: taxTreatments
  employer       TEXT NOT NULL DEFAULT ''
);

-- Where money comes from or goes. A tree: Food > Groceries.
CREATE TABLE IF NOT EXISTS categories (
  seq          INTEGER PRIMARY KEY,
  id           TEXT NOT NULL UNIQUE,           -- category_000001
  parent_id    TEXT REFERENCES categories(id),
  name         TEXT NOT NULL,
  kind         TEXT NOT NULL,                  -- vocabulary.go: categoryKinds
  role         TEXT,                           -- vocabulary.go: categoryRoles; NULL for a category the operator made
  archived_at  INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS categories_role ON categories(role) WHERE role IS NOT NULL;

CREATE TABLE IF NOT EXISTS payees (
  seq                  INTEGER PRIMARY KEY,
  id                   TEXT NOT NULL UNIQUE,   -- payee_000001
  name                 TEXT NOT NULL,
  default_category_id  TEXT REFERENCES categories(id),
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);

-- Bank text that names a payee: "AMZN MKTP" -> Amazon. Longest match wins.
CREATE TABLE IF NOT EXISTS payee_match_rules (
  seq         INTEGER PRIMARY KEY,
  id          TEXT NOT NULL UNIQUE,            -- payee_match_rule_000001
  payee_id    TEXT NOT NULL REFERENCES payees(id),
  pattern     TEXT NOT NULL UNIQUE,            -- matched case-insensitively as a substring
  created_at  INTEGER NOT NULL
);

-- One file from one source. Undoing a batch removes every transaction it made.
CREATE TABLE IF NOT EXISTS import_batches (
  seq          INTEGER PRIMARY KEY,
  id           TEXT NOT NULL UNIQUE,           -- import_batch_000001
  source       TEXT NOT NULL,                  -- vocabulary.go: importSources
  account_id   TEXT NOT NULL REFERENCES accounts(id),
  file_name    TEXT NOT NULL DEFAULT '',
  file_id      TEXT NOT NULL DEFAULT '',       -- file-store id of the original file
  file_sha256  TEXT NOT NULL,
  first_row_on TEXT NOT NULL DEFAULT '',     -- earliest row date in the file: what the file covers
  last_row_on  TEXT NOT NULL DEFAULT '',
  imported_at  INTEGER NOT NULL,
  undone_at    INTEGER NOT NULL DEFAULT 0
);

-- Each source row exactly as it came, so nothing a source said is lost and a
-- re-import can be checked against it.
CREATE TABLE IF NOT EXISTS import_rows (
  seq              INTEGER PRIMARY KEY,
  id               TEXT NOT NULL UNIQUE,       -- import_row_000001
  import_batch_id  TEXT NOT NULL REFERENCES import_batches(id),
  row_number       INTEGER NOT NULL,           -- 1 = the first row after the header
  dedupe_key       TEXT NOT NULL,              -- see importing.go: rowDedupeKey
  outcome          TEXT NOT NULL,              -- vocabulary.go: importRowOutcomes
  raw_json         TEXT NOT NULL,              -- header -> value, every column
  UNIQUE (import_batch_id, row_number)
);
CREATE INDEX IF NOT EXISTS import_rows_dedupe_key ON import_rows(dedupe_key);

CREATE TABLE IF NOT EXISTS transactions (
  seq          INTEGER PRIMARY KEY,
  id           TEXT NOT NULL UNIQUE,           -- transaction_000001
  occurred_on  TEXT NOT NULL,
  status       TEXT NOT NULL,                  -- vocabulary.go: transactionStatuses
  payee_id     TEXT REFERENCES payees(id),
  description  TEXT NOT NULL DEFAULT '',       -- the bank's text
  memo         TEXT NOT NULL DEFAULT '',       -- the operator's
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS transactions_occurred_on ON transactions(occurred_on);

-- Which source rows a transaction came from. A transfer between two of the
-- operator's accounts arrives as one row in each account's file; after
-- pairing, both rows point at the one transaction.
CREATE TABLE IF NOT EXISTS transaction_sources (
  transaction_id  TEXT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  import_row_id   TEXT NOT NULL UNIQUE REFERENCES import_rows(id),
  PRIMARY KEY (transaction_id, import_row_id)
);

CREATE TABLE IF NOT EXISTS postings (
  seq             INTEGER PRIMARY KEY,
  id              TEXT NOT NULL UNIQUE,        -- posting_000001
  transaction_id  TEXT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  account_id      TEXT REFERENCES accounts(id),
  category_id     TEXT REFERENCES categories(id),
  amount_cents    INTEGER NOT NULL,
  memo            TEXT NOT NULL DEFAULT '',
  tax_year        INTEGER NOT NULL DEFAULT 0,  -- retirement contribution year, 0 = none
  CHECK ((account_id IS NULL) <> (category_id IS NULL))
);
CREATE INDEX IF NOT EXISTS postings_transaction ON postings(transaction_id);
CREATE INDEX IF NOT EXISTS postings_account ON postings(account_id);
CREATE INDEX IF NOT EXISTS postings_category ON postings(category_id);
-- The store refuses any write that leaves a transaction's postings summing to
-- anything but zero; SQLite cannot check that across rows.

-- When the transfer matcher folds one transaction into another, the folded
-- id is kept here so a reference to it can still be followed.
CREATE TABLE IF NOT EXISTS transaction_merges (
  merged_transaction_id  TEXT PRIMARY KEY,
  into_transaction_id    TEXT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  reason                 TEXT NOT NULL,
  merged_at              INTEGER NOT NULL
);

-- A posting the transfer matcher moved onto an account with no partner row
-- to confirm it: the account has no feed, or its feed starts later than the
-- row. An import into that account covering the posting's date is refused
-- until the move is undone, or the transfer would count twice.
CREATE TABLE IF NOT EXISTS transfers_without_partner (
  posting_id      TEXT PRIMARY KEY REFERENCES postings(id) ON DELETE CASCADE,
  transaction_id  TEXT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  account_id      TEXT NOT NULL REFERENCES accounts(id),
  reason          TEXT NOT NULL,
  created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS statements (
  seq                    INTEGER PRIMARY KEY,
  id                     TEXT NOT NULL UNIQUE, -- statement_000001
  account_id             TEXT NOT NULL REFERENCES accounts(id),
  period_start           TEXT NOT NULL,
  period_end             TEXT NOT NULL,
  closing_balance_cents  INTEGER NOT NULL,
  minimum_due_cents      INTEGER NOT NULL DEFAULT 0,
  due_on                 TEXT NOT NULL DEFAULT '',
  file_id                TEXT NOT NULL DEFAULT '',
  created_at             INTEGER NOT NULL
);

-- "The statement says the balance was this at the end of that day." The store
-- reports any difference from the sum of its own postings.
CREATE TABLE IF NOT EXISTS balance_assertions (
  seq            INTEGER PRIMARY KEY,
  id             TEXT NOT NULL UNIQUE,         -- balance_assertion_000001
  account_id     TEXT NOT NULL REFERENCES accounts(id),
  as_of          TEXT NOT NULL,
  balance_cents  INTEGER NOT NULL,
  source         TEXT NOT NULL,                -- free text: "statement_000004", "Rocket Money 2026-11-06"
  created_at     INTEGER NOT NULL
);

-- Bills and subscriptions expected to come again.
CREATE TABLE IF NOT EXISTS recurring_charges (
  seq             INTEGER PRIMARY KEY,
  id              TEXT NOT NULL UNIQUE,        -- recurring_charge_000001
  payee_id        TEXT NOT NULL REFERENCES payees(id),
  account_id      TEXT REFERENCES accounts(id),
  category_id     TEXT REFERENCES categories(id),
  expected_cents  INTEGER NOT NULL,
  rrule           TEXT NOT NULL,               -- RFC 5545: FREQ=MONTHLY;BYMONTHDAY=3
  next_on         TEXT NOT NULL,
  ended_on        TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

-- One amount per category per month. The limit view compares it with that
-- month's spending; the envelope view carries what is left into later months
-- and subtracts every assignment from income to get "ready to assign".
CREATE TABLE IF NOT EXISTS budget_assignments (
  seq           INTEGER PRIMARY KEY,
  id            TEXT NOT NULL UNIQUE,          -- budget_assignment_000001
  category_id   TEXT NOT NULL REFERENCES categories(id),
  month         TEXT NOT NULL,                 -- 'YYYY-MM'
  amount_cents  INTEGER NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (category_id, month)
);

-- A pointer from one of this store's records to a record another store owns,
-- by that store's id: a receipt in file-store, a person in people-store.
CREATE TABLE IF NOT EXISTS links (
  seq          INTEGER PRIMARY KEY,
  id           TEXT NOT NULL UNIQUE,           -- link_000001
  from_id      TEXT NOT NULL,
  entity_type  TEXT NOT NULL,                  -- kanban-store entity-type registry name
  entity_ref   TEXT NOT NULL,
  note         TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  UNIQUE (from_id, entity_type, entity_ref)
);

-- A model's suggested category for one transaction, waiting for the
-- operator. Accepting one writes the category onto the transaction; nothing
-- else does. A newer suggestion for the same transaction supersedes an older
-- pending one.
CREATE TABLE IF NOT EXISTS category_suggestions (
  seq                    INTEGER PRIMARY KEY,
  id                     TEXT NOT NULL UNIQUE,   -- category_suggestion_000001
  transaction_id         TEXT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  category_id            TEXT REFERENCES categories(id), -- NULL: the model said a person must decide
  confidence             REAL NOT NULL,          -- the model's own estimate, 0 to 1
  rationale              TEXT NOT NULL DEFAULT '',
  needs_review           INTEGER NOT NULL DEFAULT 0,
  model                  TEXT NOT NULL,          -- what answered, from the operation receipt
  operation_id           TEXT NOT NULL,          -- llm-bridge-server operation that produced it
  status                 TEXT NOT NULL,          -- vocabulary.go: suggestionStatuses
  decided_category_id    TEXT REFERENCES categories(id), -- what the operator chose, when they changed it
  decided_at             INTEGER NOT NULL DEFAULT 0,
  created_at             INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS category_suggestions_status ON category_suggestions(status, transaction_id);
