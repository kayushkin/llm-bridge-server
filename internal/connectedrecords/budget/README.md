# budget-store

A personal finance ledger: accounts, credit cards, loans, retirement
accounts, every transaction, categories and budgets.

It keeps books by double entry. Every transaction holds two or more postings
that sum to zero, so paying a credit card from checking moves money between
two accounts instead of counting as spending, and every account's balance can
be checked against its statement. Amounts are integer cents everywhere.

- Imports bank CSVs and keeps every source row exactly as it came; a
  re-downloaded overlap is caught, and a whole file can be undone.
- Pairs the two sides of a transfer between your own accounts into one
  transaction, only when one side's description names the other account.
- Reads one set of monthly budget amounts two ways: a per-month limit, or
  envelopes whose leftovers roll forward.

## Where it runs

budget-store runs inside llm-bridge-server, at `/budget/`, with the
paths it served as a standalone service on `:8340`. Its books are in
Postgres, in the schema `budget` owned by the role `budget_module`
(`Module()`, `migrations/`); the server provisions and migrates it and hands
the pool to `New(db)`.

## Owners

Every row has an owner, `owner_principal_id`, and each person keeps books of
their own. `Store.OpenLedger(ctx, owner)` opens one owner's books on one
connection, and Postgres row level security narrows every statement on it to
that owner's rows, so a query cannot reach another owner's money by leaving
a condition off. Foreign keys run over (owner, id), so a posting cannot land
on another owner's account, and a deferred trigger refuses an unbalanced
transaction at commit. CONTRACT.md *Owners* says which books each route
works in.

## Commands

`cmd/budget-suggest-categories` and `cmd/budget-match-trips` are HTTP
clients of the routes. The server also runs the matching passes itself,
each off until its setting turns it on (llm-bridge-server's
`budget_store.*` settings): bank sync through Plaid, recurring bill
detection, interest matching and trip matching, which reads location-store's
journeys in the same process.

```bash
go test ./internal/connectedrecords/budget/...   # needs POSTGRESTEST_ADMINISTRATOR_URL
llm-bridge-server import-budget-store -sqlite <copy of budget-store.db> -owner <principal id>
```
