# budget-store

A personal finance ledger: accounts, credit cards, loans, retirement
accounts, every transaction, categories and budgets. Go, SQLite, one HTTP
service on loopback.

It keeps books by double entry. Every transaction holds two or more postings
that sum to zero, so paying a credit card from checking moves money between
two accounts instead of counting as spending, and every account's balance can
be checked against its statement.

- Imports bank CSVs and keeps every source row exactly as it came; a
  re-downloaded overlap is caught, and a whole file can be undone.
- Pairs the two sides of a transfer between your own accounts into one
  transaction, only when one side's description names the other account.
- Reads one set of monthly budget amounts two ways: a per-month limit, or
  envelopes whose leftovers roll forward.

`CONTRACT.md` is the API. `schema.sql` is the database.

```bash
go test ./...
go build ./cmd/budget-store && ./budget-store   # listens on 127.0.0.1:8340
```
