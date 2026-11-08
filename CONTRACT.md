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
`budget_assignment_000001`, `link_000001`, `category_suggestion_000001`,
`marketplace_order_000001`, `marketplace_order_line_000001`. The prefixes let kanban-store's
entity-type registry match each kind for chips: `budget_account`,
`budget_transaction`, `budget_category`, `budget_payee` and
`budget_import_batch` resolve through the matching `GET …/{id}`.

## Routes

| Route | Does |
|---|---|
| `GET /health`, `GET /settings`, `GET /vocabulary` | liveness; declared settings; every closed set of values the store accepts |
| `GET/POST /institutions` | banks and issuers: `{name, website}` |
| `GET/POST /accounts`, `GET/PATCH /accounts/{id}` | accounts with `balance_cents` and their detail block (`credit_card`, `loan`, `retirement`). A card's `interest_category_id` and `fee_category_id`, and a loan's `interest_category_id`, name by id the expense categories its interest and fees are filed under, so a report finds them without matching names. `last_four` must be four digits: a full number is refused. Setting it also adds it as a description marker |
| `POST /accounts/{id}/description-markers`, `DELETE /accounts/{id}/description-markers/{marker_id}` | `{marker, note}`: text that names this account in another account's rows (an old card number, a loan reference). At least 4 characters. Deleting one leaves what it already matched |
| `POST /accounts/{id}/opening-balance` | `{as_of, balance_cents, source}`: makes the ledger agree with a reported balance, with one opening-balance transaction dated the day before the account's first posting; replaces any earlier one. Also records an assertion |
| `GET /accounts/{id}/loan-schedule` | a loan's every payment split into interest and principal, and the projection to payoff; see Loans. Changes nothing |
| `POST /accounts/{id}/loan-schedule/apply` | writes the schedule onto the payments: the loan gets the principal, `loan.interest_category_id` the interest. Run again after a new fact or payment |
| `GET/POST /accounts/{id}/loan-payment-facts` | `{facts:[{effective_on, interest_cents, principal_cents, principal_balance_after_cents, source}]}`: what a statement says about one payment; replaces one on the same date |
| `GET/POST /balance-assertions` | `{account_id, as_of, balance_cents, source}`: a reported balance; each answer carries `ledger_balance_cents` and `difference_cents` (ledger minus reported) as the ledger stands now |
| `GET/POST /categories`, `GET /categories/{id}` | `{parent_id, name, kind}`; kind `expense`, `income` or `equity`. Two categories carry a `role` the store posts to: `uncategorized` and `opening_balances`. Find them by role, never by name |
| `GET/POST /payees`, `GET /payees/{id}`, `POST /payees/apply-rules` | `{name, default_category_id, patterns}`: a pattern is upper-cased text matched as a substring of a description; the longest match wins. `apply-rules` names every transaction without a payee and moves one still wholly Uncategorized to the payee's default category. A category someone chose is never overwritten |
| `GET /transactions` | newest first; `account_id`, `category_id` (with `include_subcategories=true`, every category under it too), `month`, `q`, `uncategorized=true`, `limit` (≤1000, default 200), `offset` |
| `POST /transactions` | `{occurred_on, status, payee_id, description, memo, postings}` by hand |
| `GET/PATCH /transactions/{id}` | Every transaction read carries `category_decision`: the newest accepted or edited suggestion (`suggestion_id`, `status`, `decided_by`, `confidence`, `model`), or null when its category came another way. PATCH `{status, payee_id, memo}`. The id of a transaction the transfer matcher folded away answers with the one it was folded into |
| `PUT /transactions/{id}/categories` | `{splits: [{category_id, amount_cents, memo, tax_year}]}` replaces every category posting; the splits must sum to what the category side summed to, so accounts never change |
| `POST /transfers/match` | pairs transfers between the operator's own accounts; see below |
| `POST /transactions/{id}/move-to-account` | `{account_id, reason}`: the transaction's one category posting becomes a posting on that account, for money that in fact moved to or from one of the operator's accounts (a loan paid out into checking). Kept in `transfers_without_partner` with the reason, so it blocks an overlapping import and can be undone like the matcher's. 409 unless the transaction has exactly one account and one category posting |
| `POST /transactions/{id}/undo-transfer-without-partner` | puts a posting the matcher moved without a partner back on Uncategorized |
| `POST /import-batches?source=&account_id=&file_name=&file_id=` | body: the file. One transaction per new row; all or nothing. `file_id` is the file-store id of the original |
| `GET /import-batches`, `GET /import-batches/{id}`, `POST /import-batches/{id}/undo` | `outcomes` counts each row's fate, `first_row_on`/`last_row_on` the dates the file covers; undo deletes what the batch made and keeps its rows |
| `GET /reports/spending?month=` | each category's total that month |
| `GET /reports/monthly?from=&to=` | each category's total in each month from `from` to `to` (YYYY-MM, inclusive, either optional): `[{month, category_id, kind, total_cents, transactions}]` |
| `GET /reports/by-account?category_id=…&category_id=…&from=&to=` | those categories and every one under them, per month, category and account of the transaction: card interest per card per month |
| `GET /budget?month=&view=limit\|envelope`, `PUT /budget-assignments` | see below |
| `GET /links?from_id=`, `POST /links` | `{from_id, entity_type, entity_ref, note}`: a pointer to another store's record by that store's id. It starts at an account, transaction, payee, category, statement, marketplace order or order line |
| `GET/POST /marketplace-orders`, `GET /marketplace-orders/{id}`, `POST /marketplace-orders/match-transactions`, `GET/PATCH /marketplace-order-lines/{id}` | see Marketplace orders |

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

## Loans

`GET /accounts/{id}/loan-schedule` assumes simple interest charged daily on
the principal owed: a payment's interest is principal × `loan.apr_basis_points`
× days since the last payment / 365, rounded half up to the cent. The payout
is the loan's one transfer into another account; payments are its transfers
from another account, each dated on `loan.first_payment_on`'s day of month
when it posted within 7 days of it. A statement's own split
(`loan_payment_facts`) is used for its payment instead, and where computed
payments meet one, the last computed payment moves the difference between
interest and principal so the principal owed matches the statement, and says
so. Each row's `source` is `statement`, `computed`, `computed, adjusted to meet
a statement`, or `projected` (the scheduled payment on each due day until
payoff). `schedule_applied_to_all` says whether the postings already carry it.

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
reads `GET /category-suggestions/candidates?before=` (wholly Uncategorized
transactions with no pending suggestion, dated before `before` when given), offers every expense and income
category by its path (`GET /category-paths`) plus "Needs the operator", and
runs each batch as one `classification.run` operation on llm-bridge-server
(role `best` by default) for a principal-store group. It records the answers
with `POST /category-suggestions {operation_id, model, suggestions:
[{transaction_id, category_id, confidence, rationale, needs_review}]}`; an
empty `category_id` means a person must decide. It writes no category itself. `-must-choose` drops "Needs the operator", for when the operator asked for a best guess on everything; `-before` limits it to older transactions.

| Route | Does |
|---|---|
| `GET /category-suggestions?status=` | with each transaction's date, description, amount and account; statuses in `GET /vocabulary` |
| `POST /category-suggestions/decisions` | `{decided_by, decisions: [{suggestion_id, action: accept\|reject, category_id}]}`. `decided_by` is required: who decided, in their words (dash writes the signed-in operator), kept on every suggestion it decides, so a category the operator chose can be told from one they let a model guess. Accept writes the suggested category, or `category_id` when given (status `edited`), onto the transaction's Uncategorized posting. A transaction someone already sorted or split is left alone and its suggestion goes `stale`. Deciding one that is not pending is a 409. All or nothing |

A newer suggestion for a transaction supersedes its pending one; a rejected
one leaves the transaction a candidate again.

## Recurring bills

`POST /recurring-bills/detect` groups payments by payee (or, with none, by the
description's first words once reference numbers and dates are dropped) and
looks for a steady rhythm: weekly, monthly, quarterly or yearly, with most
gaps between payments in that rhythm's band. A payment is money spent (a
positive expense or Uncategorized posting) or a payment into a loan from
another account, interest included; paying a credit card is never a bill. A
new bill is `suggested` only while it is still being paid; a known one keeps
the operator's decision, has its payments, usual amount (median of the last
three) and dates refreshed, goes `ended` when confirmed and unpaid past its
next due date plus grace, and back to `confirmed` when paid again. Answers
`{suggested, updated, ended, resumed}`.

| Route | Does |
|---|---|
| `POST /recurring-bills` | `{transaction_id, cadence, status: confirmed\|suggested, name, decided_by}`: makes a bill of the payments the transaction belongs to (same payee, or the same words naming who was paid). The cadence is the caller's, so no rhythm is tested and several charges in one period are that period's payment. 409 when those payments are already a bill |
| `GET /recurring-bills?status=` | soonest due first, each with `payment_count` and `monthly_equivalent_cents` |
| `GET /recurring-bills/{id}` | with every payment, oldest first, and each one's change from the one before |
| `PATCH /recurring-bills/{id}` | `{status: confirmed\|rejected, name, decided_by}`; a status needs `decided_by` |
| `GET /recurring-bills/totals` | confirmed bills: expected per month, six months and year, and what they actually came to in the last six and twelve months |

## Trips

A trip is location-store's (`trip_000001`); this store keeps which
transactions belong to it, by that id, in `trip_transactions`.
`cmd/budget-match-trips` reads location-store's journeys and, for each, calls
`POST /trips/{trip_ref}/suggest {first_day, last_day, home_markers}`: spending
dated from the first day to three days after the last (card charges post
late) is suggested, except payments of confirmed or suggested recurring bills,
charges whose text holds a home marker (the caller's data, such as a city and
state as the bank prints them), and transactions already linked to this trip
or confirmed on another. Removed links are never suggested again.

| Route | Does |
|---|---|
| `GET /trips/totals` | per trip with links: confirmed and suggested spending and counts |
| `GET /trips/{trip_ref}/transactions` | its links oldest first, removed ones included, each with date, text, amount, category and account |
| `PUT /trips/{trip_ref}/transactions/{transaction_id}` | `{status: confirmed\|removed, decided_by, reason}`: decide a suggestion, or add a transaction the dates missed. Confirming drops the transaction's suggestions on other trips; one confirmed on another trip is a 409 |

## Interests

An interest (`interest_000001`) gathers one pursuit's spending across
categories. Its rules (`interest_rules`) match a category and every category
under it, a payee, or upper-cased text in the description; a rule's matches
are confirmed at once, because the rules are the operator's. The operator can
add a transaction no rule caught or remove one; a removed one stays removed
when the rules run again. History sources point at another store's record of
the pursuit: a location-store place (its visits) or a calendar-store search.

| Route | Does |
|---|---|
| `GET/POST /interests`, `GET /interests/{id}` | `{name, description}`; each read carries its rules, history sources, and confirmed total and count |
| `POST /interests/{id}/rules` | `{kind: category\|payee\|description, value}`: adds the rule and confirms what it matches |
| `POST /interests/{id}/history-sources` | `{kind: location_place\|calendar_search, ref, note}`; a place ref must be `place_000001` |
| `GET /interests/{id}/transactions`, `PUT /interests/{id}/transactions/{transaction_id}` | newest first, removed included; PUT `{status: confirmed\|removed, decided_by}` |
| `POST /interests/match` | runs every interest's rules again for new transactions; answers `{added}` |

## Marketplace orders

An order placed on a marketplace (`marketplaces` in `GET /vocabulary`: `ebay`)
and what each line bought, as its order confirmation says; the charge is its
transaction. possession-store's eBay import reads the confirmations from
mailstack and posts them here.

`POST /marketplace-orders {marketplace, order_number, seller, ordered_at,
total_cents, currency, mail_account_id, mail_message_id, extracted_by,
created_by, lines:[{title, item_number, listing_url, quantity,
unit_price_cents, disposition}]}` → **201**. `ordered_at` is epoch seconds,
money is cents in `currency` (ISO 4217; the ledger is USD, an order may not
be), `mail_account_id` and `mail_message_id` are the confirmation's mailstack
account and provider message id (dash opens it at
`/mail?account=&message=`), `extracted_by` the model that read it. An order
already recorded — the same marketplace and order number, or the same email
— is a **409** naming it. The store finds the charge itself and the caller
never sends one:

1. `order_number`: the one transaction whose description holds the order
   number (`EBAY O*19-14929-85390`).
2. `amount_and_date`: else, for a USD total, the one transaction not already
   an order's, dated from the day before the order to 7 days after, whose
   account postings sum to minus the total and whose description names the
   marketplace (`EBAY`), as card charges print it (`Ebay Commerce Inc.`).

Neither, or more than one: `transaction_id` stays empty (`matched_by` `""`):
an order older than the ledger has no charge here. `POST
/marketplace-orders/match-transactions` tries every such order again, after
an import, and answers `{order_number, amount_and_date, unmatched}`.

`GET /marketplace-orders?transaction_id=&unmatched=true&limit=&offset=`,
newest first. Every transaction read carries `marketplace_orders` (the orders
it paid, with lines) and `links` (from it), so its chip can show what it
bought. A line answers `possession_ids`, the `possession` links from it;
possession-store writes those, and the possession points back at the
transaction by `budget_transaction_id`. `PATCH /marketplace-order-lines/{id}
{disposition}`: `unreviewed`, `possession`, `consumable`, `returned` or
`gift_for_someone_else`.

## Not yet served

`statements` and `recurring_charges` (the first draft of recurring bills, unused) have tables and no routes.
