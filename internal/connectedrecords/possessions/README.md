# possessions (formerly possession-store)

`internal/connectedrecords/possessions`, package `possessions`. It was the standalone possession-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `possessions`. Its record ids did not change.

An inventory of the things a person owns: clothes, computers, audio gear,
instruments, vehicles and everything else, with photos, prices and where each
thing is kept. It runs inside llm-bridge-server, at `/possessions/`, and
keeps its records in Postgres, each owned by one person.

- **Kinds.** Every possession has the same core record. Each kind (clothes,
  computers, computer parts, audio devices, instruments, vehicles and fourteen
  more) has a table of its own for the facts only that kind has, and its own
  id prefix.
- **Outfits** are sets of wearables, read from photos of the owner.
- **Photo review.** Two models read each photo of a room on their own, then
  see each other's answers and settle on one list of what is in it. Where
  they still differ, or cannot tell a new thing from one already recorded,
  the store asks the owner a question.
- **Purchases** are kept line by line; eBay orders are budget-store's.

`CONTRACT.md` is the API. `cmd/possession-add-photos` brings photos in
through the bridge and queues them for review.
