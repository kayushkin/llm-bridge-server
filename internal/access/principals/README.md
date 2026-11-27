# principal-store

The registry of *principals* — the humans and groups a permission can be
granted to and a card can be assigned to — as a package of llm-bridge-server,
`internal/access/principals`. It came in with its history from the
standalone `principal-store` repository, which served the same routes at the
root of `127.0.0.1:8314` from a SQLite file. The bridge mounts every route
under `/principal-store/` with the same paths behind the prefix, and decides
who may call each one; this package checks no caller.

## Why it exists

Nothing on this box models a person. The operator appears as `dana`, `kanban`
and `dana` in three different stores with nothing joining them, and kanban has
no assignee concept at all. This store hands out the id — `principal_000001` —
that every other store joins on. Display names and emails ride alongside for
display; neither is unique and neither is ever a join key.

## Where the records live

Postgres, in the module `principals` (role `principals_module`), through the
bridge's `postgresstorage` package: `Module()` names it and its embedded
`migrations/*.sql`, the server provisions, migrates and opens it, and `New`
takes the pool. Ids are the identity column's number through
`FormatRecordID`/`ParseRecordID` (`principal_000001`, `timeoff_000001`,
`identity_binding_000001`), and only that exact form names a row.
`ImportStandaloneDatabase` copies a standalone store's SQLite file into an
empty module, keeping every id, number, time, disabled principal and unlinked
binding, and refuses a file it cannot copy whole.

What changed in the move:

- Search is a `tsvector` with a GIN index instead of FTS5. Bare words are still
  prefixes, a query with search syntax still means exactly what it says
  (quoted phrases, `word*`, `AND`, `OR`, `NOT`, parentheses, `display_name:` /
  `email:` column filters), words still split at every ASCII punctuation mark
  and diacritics still come off. `NEAR`, `{column lists}` and `^` are a 400; a
  quoted phrase or a parenthesis with no word is a 400; punctuation alone in a
  plain query matches nothing, as before.
- A time-off number is never handed out again after its row is deleted; the
  standalone store reused the highest one.
- `GET /settings` is gone (the bridge's own settings describe it), and so are
  the command, unit and deploy script.

## Use it

```bash
# Through the bridge, with a credential its route rules accept (send the service token or a login):
B=http://127.0.0.1:8160/principal-store
curl -s -X POST $B/principals \
  -H 'Content-Type: application/json' \
  -d '{"kind":"human","display_name":"Priya Raman","email":"priya.raman@northwind-eng.example"}'

curl -s -X POST $B/principals \
  -H 'Content-Type: application/json' -d '{"kind":"group","display_name":"Data Team"}'

curl -s -X PUT $B/principals/principal_000002/members/principal_000001   # 201, then 200
curl -s "$B/principals?q=pri"                                             # prefix search
curl -s $B/principals/principal_000001                                    # with its groups
curl -s -X POST $B/principals/principal_000001/disable                    # the only removal

# The week, in the person's own zone; then who in a group is working right now
curl -s -X PATCH $B/principals/principal_000001 -H 'Content-Type: application/json' \
  -d '{"availability":{"tzid":"Europe/Amsterdam","days":["MO","TU","WE","TH","FR"],"start":"09:00","end":"17:00"}}'
curl -s -X POST $B/principals/principal_000001/time-off -H 'Content-Type: application/json' \
  -d '{"starts_at":1790000000,"ends_at":1790600000,"note":"holiday"}'
curl -s "$B/principals/principal_000001/availability"                     # {available, reason}
curl -s "$B/principals/principal_000002/members?available_at="            # the assignment-pool read
```

`CONTRACT.md` is the route table and the field-by-field reference.

## Design notes

**There is no hard delete.** A principal that vanished would leave dangling
assignee ids in kanban and dangling grantees in permission-store, so
`disabled_at` is the only removal: the row stays, drops out of default listings,
and `GET /principals/{id}` still answers so a card assigned to a departed human
still renders.

**Kind is fixed at creation and memberships are one level deep.** A group holds
humans and nothing else in v1 — no nested groups — because every consumer that
expands a group can then do it with one query and no cycle check. `PATCH` with
`kind` is a 400 saying so.

**What a principal may use lives in grant-store, not here.** This store is the
directory; `grant-store` (`:8315`) holds the tuples — `can_use`, `can_run_as`,
`can_dispatch_on`, and the advisory `works_with` list this store carried as
`principal_resources` for one day (2026-09-10 to 2026-09-11). grant-store reads
a human's groups from here on every effective-set call, so a membership change
shows at the next session start.

**A principal with no declared week is unknown, and unknown is never
available.** `availability` is the human's own working week in their own zone
(`tzid` must be a real zone, never an offset — an offset drifts an hour at every
DST change), `time_off` rows sit beside it, and
`GET /principals/{id}/availability` reduces both to `{available, reason}` with
a served reason vocabulary. There is no default zone and no measured presence:
the first consumer is the Northwind simulation, so the declared week is the
whole signal, and kanban-store reads
`GET /principals/{group}/members?available_at=` to pick an assignee who is
actually working.

**Search is prefix-by-default.** `?q=pri` finds Priya Raman, because the main
caller is an assignee picker reading keystrokes. A query that carries search
syntax — a quoted phrase, `*`, a column filter, an upper-case operator — means
exactly what it says (`search_query.go`).

## License

MIT — see [LICENSE](LICENSE).

## The administrator

`is_administrator` on a human is the one fact that grants access to boards and
sessions nobody granted: kanban-store, grant-store and llm-bridge-server let an
administrator past every check they make. Set it with
`PATCH /principals/{id} {"is_administrator":true}`; a group is refused it.

## Contacts

`contact` is the third kind: the outside person a ticket is from. It cannot log
in, hold a grant, join a group or administer anything. `POST /contacts/resolve`
turns an email address into one contact id — 201 when it creates the contact,
200 when it finds it — so a ticket joins on an id rather than on an address.
