# budget-store: API contract

`127.0.0.1:8340`. The operator's finance ledger: accounts, cards, loans,
retirement accounts, every transaction, categories and budgets.

Module `github.com/kayushkin/budget-store`, root package `budgetstore`, server
in `cmd/budget-store`. SQLite at `<data dir>/budget-store.db` (mode 600, its
directory 700), WAL, foreign keys on.

**No auth, so the bind is `127.0.0.1` and that is part of this contract.**
`deploy.sh` fails if the port listens anywhere else. Bodies are JSON, except
an import, whose body is the file. Timestamps are epoch seconds; dates are
`YYYY-MM-DD`; months are `YYYY-MM`. Errors are `{"error": "…"}`.

| Status | Means |
|---|---|
| 400 | the body or a parameter is wrong, including an unknown JSON key on any write, and a transaction whose postings do not sum to zero |
| 404 | no such record |
| 409 | the write contradicts a row: a file already imported, a batch already undone, a pattern another payee owns |

## Double entry

A **transaction** holds two or more **postings** that sum to zero. A posting
names exactly one of `account_id` and `category_id`. Amounts are integer
cents, USD.

- A posting on an account is the change in that account's balance. An
  asset's balance is positive; a debt's (`is_liability`: credit cards, loans)
  is negative. A card purchase is negative on the card; paying the card is
  positive on it.
- A posting on a category is the other side: spending is positive on an
  expense category, income negative on an income category.
- So paying a card from checking is −X on checking and +X on the card, with
  no category: it is not spending. The purchases were counted when they hit
  the card.

## Ids

A prefix and a zero-padded sequence: `institution_000001`, `account_000001`,
`account_description_marker_000001`, `category_000001`, `payee_000001`,
`payee_match_rule_000001`, `import_batch_000001`, `import_row_000001`,
`transaction_000001`, `posting_000001`, `balance_assertion_000001`,
`budget_assignment_000001`, `link_000001`, `category_suggestion_000001`. The prefixes let kanban-store's
entity-type registry match each kind for chips: `budget_account`,
`budget_transaction`, `budget_category`, `budget_payee` and
`budget_import_batch` resolve through the matching `GET …/{id}`.

## Routes

| Route | Does |
|---|---|
| `GET /health`, `GET /settings`, `GET /vocabulary` | liveness; declared settings; every closed set of values the store accepts |
| `GET/POST /institutions` | banks and issuers: `{name, website}` |
| `GET/POST /accounts`, `GET/PATCH /accounts/{id}` | accounts with `balance_cents` and their detail block (`credit_card`, `loan`, `retirement`). `last_four` must be four digits: a full number is refused. Setting it also adds it as a description marker |
| `POST /accounts/{id}/description-markers`, `DELETE /accounts/{id}/description-markers/{marker_id}` | `{marker, note}`: text that names this account in another account's rows (an old card number, a loan reference). At least 4 characters. Deleting one leaves what it already matched |
| `POST /accounts/{id}/opening-balance` | `{as_of, balance_cents, source}`: makes the ledger agree with a reported balance, with one opening-balance transaction dated the day before the account's first posting; replaces any earlier one. Also records an assertion |
| `GET/POST /balance-assertions` | `{account_id, as_of, balance_cents, source}`: a reported balance; each answer carries `ledger_balance_cents` and `difference_cents` (ledger minus reported) as the ledger stands now |
| `GET/POST /categories`, `GET /categories/{id}` | `{parent_id, name, kind}`; kind `expense`, `income` or `equity`. Two categories carry a `role` the store posts to: `uncategorized` and `opening_balances`. Find them by role, never by name |
| `GET/POST /payees`, `GET /payees/{id}`, `POST /payees/apply-rules` | `{name, default_category_id, patterns}`: a pattern is upper-cased text matched as a substring of a description; the longest match wins. `apply-rules` names every transaction without a payee and moves one still wholly Uncategorized to the payee's default category. A category someone chose is never overwritten |
| `GET /transactions` | newest first; `account_id`, `category_id`, `month`, `q`, `uncategorized=true`, `limit` (≤1000, default 200), `offset` |
| `POST /transactions` | `{occurred_on, status, payee_id, description, memo, postings}` by hand |
| `GET/PATCH /transactions/{id}` | PATCH `{status, payee_id, memo}`. The id of a transaction the transfer matcher folded away answers with the one it was folded into |
| `PUT /transactions/{id}/categories` | `{splits: [{category_id, amount_cents, memo, tax_year}]}` replaces every category posting; the splits must sum to what the category side summed to, so accounts never change |
| `POST /transfers/match` | pairs transfers between the operator's own accounts; see below |
| `POST /transactions/{id}/undo-transfer-without-partner` | puts a posting the matcher moved without a partner back on Uncategorized |
| `POST /import-batches?source=&account_id=&file_name=&file_id=` | body: the file. One transaction per new row; all or nothing. `file_id` is the file-store id of the original |
| `GET /import-batches`, `GET /import-batches/{id}`, `POST /import-batches/{id}/undo` | `outcomes` counts each row's fate, `first_row_on`/`last_row_on` the dates the file covers; undo deletes what the batch made and keeps its rows |
| `GET /reports/spending?month=` | each category's total that month |
| `GET /budget?month=&view=limit\|envelope`, `PUT /budget-assignments` | see below |
| `GET /links?from_id=`, `POST /links` | `{from_id, entity_type, entity_ref, note}`: a pointer to another store's record by that store's id |

## Importing

Sources (`GET /vocabulary` `import_sources`): `wells_fargo_csv` (Download
Account Activity, comma delimited, with its header) and `inspira_csv` (my
transactions). Every source row is kept as `raw_json`, every column, with an
outcome:

- `created`: a transaction with the account's posting and the opposite amount
  on the matching payee's default category, or on Uncategorized.
- `duplicate`: an earlier live batch already holds this row. A row is the same
  row when source, account, date, description, amount and its occurrence
  number among identical rows in its file all match: two real $3 coffees on
  one day stay two, while a re-downloaded overlap is caught.
- `skipped_internal`: a move inside the account that changes no balance, such
  as Inspira's cash-sweep buys and sells.

Importing the same file (same sha256) into the same account twice is a 409.

## Transfers

`POST /transfers/match` answers `{paired, assigned_without_partner}`.

A candidate is a transaction still in its imported shape: one account posting
and one Uncategorized posting. Two candidates pair when their amounts are
opposite, they are dated at most 5 days apart, they are in different accounts,
and **one's description carries a description marker of the other's
account**. The last test is what stops a $2,000 rent payment and a $2,000 card
payment in the same week from pairing. The closest dates pair first. The pair
becomes the money-out side's transaction, holding both account postings and
both source rows; the other id is kept and still resolves.

Some transfers have no partner row to wait for: the account has
`has_own_feed: false` (a loan with no export), or its imported rows start
after the transfer (a card whose export begins when it was reissued). A
candidate whose description names such an account moves its Uncategorized
posting onto that account, and the move is kept in `transfers_without_partner`
with its reason. Only markers of 8 or more characters count here: with no
partner row there is no amount to check, and four digits turn up inside
unrelated card-terminal numbers.

**Importing into an account a file whose dates, widened by the 5-day window,
cover one of those moves is a 409** naming them, since the file may hold the
other side and the transfer would count twice. `POST
/transactions/{id}/undo-transfer-without-partner` puts the posting back on
Uncategorized; import, then match again.

## Budgets

One amount per expense category per month (`PUT /budget-assignments
{category_id, month, amount_cents}`). `GET /budget` reads them two ways:

- `limit`: `available_cents` is this month's assignment minus this month's
  spending.
- `envelope`: `available_cents` is everything ever assigned minus everything
  ever spent through this month, so what is left rolls forward; and
  `ready_to_assign_cents` is income and opening balances that reached
  on-budget accounts through this month, minus everything assigned.

Both report `uncategorized_cents`: spending neither view can count until it
is sorted.

## Category suggestions

A model suggests; only the operator decides. `cmd/budget-suggest-categories`
reads `GET /category-suggestions/candidates` (wholly Uncategorized
transactions with no pending suggestion), offers every expense and income
category by its path (`GET /category-paths`) plus "Needs the operator", and
runs each batch as one `classification.run` operation on llm-bridge-server
(role `best` by default) for a principal-store group. It records the answers
with `POST /category-suggestions {operation_id, model, suggestions:
[{transaction_id, category_id, confidence, rationale, needs_review}]}`; an
empty `category_id` means a person must decide. It writes no category itself.

| Route | Does |
|---|---|
| `GET /category-suggestions?status=` | with each transaction's date, description, amount and account; statuses in `GET /vocabulary` |
| `POST /category-suggestions/decisions` | `{decisions: [{suggestion_id, action: accept\|reject, category_id}]}`. Accept writes the suggested category, or `category_id` when given (status `edited`), onto the transaction's Uncategorized posting. A transaction someone already sorted or split is left alone and its suggestion goes `stale`. Deciding one that is not pending is a 409. All or nothing |

A newer suggestion for a transaction supersedes its pending one; a rejected
one leaves the transaction a candidate again.

## Not yet served

`statements` and `recurring_charges` have tables and no routes yet.
