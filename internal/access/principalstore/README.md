# principal-store

`127.0.0.1:8314`. The registry of *principals* — the humans and groups a
permission can be granted to and a card can be assigned to.

Part of the store family alongside prediction-store `:8313`, quote-store
`:8309` and job-store `:8311`, and the same shape as all three: one record type
per table, ids handed out here and joined on everywhere else, routes rooted at
`/`.

## Why it exists

Nothing on this box models a person. The operator appears as `slava`, `kanban`
and `slava` in three different stores with nothing joining them, and kanban has
no assignee concept at all. This store hands out the id — `principal_000001` —
that every other store joins on. Display names and emails ride alongside for
display; neither is unique and neither is ever a join key.

## Run it

```bash
./deploy.sh          # test, build, install the unit and the seed script, start, smoke-check
make check           # fmt, vet, test, build
```

The `sqlite_fts5` build tag is mandatory: a binary built without it starts and
then dies applying the schema with `no such module: fts5`. `Makefile` and
`deploy.sh` both carry it, and `Open()` names it if a binary from elsewhere
shows up.

Env: `PRINCIPAL_STORE_ADDR` (default `127.0.0.1:8314`), `PRINCIPAL_STORE_DATA_DIR`
(default `~/.config/principal-store`). SQLite at `<data dir>/principal-store.db`,
WAL, foreign keys on.

**The bind is loopback on purpose.** This service has no auth; dash is the front
door that adds it at `/api/principals`. Older siblings bind `*`, this one does
not, and `deploy.sh` fails if it finds the port listening anywhere but
`127.0.0.1`.

## Use it

```bash
curl -s -X POST http://127.0.0.1:8314/principals \
  -H 'Content-Type: application/json' \
  -d '{"kind":"human","display_name":"Priya Raman","email":"priya.raman@northwind-eng.example"}'

curl -s -X POST http://127.0.0.1:8314/principals \
  -H 'Content-Type: application/json' -d '{"kind":"group","display_name":"Data Team"}'

curl -s -X PUT http://127.0.0.1:8314/principals/principal_000002/members/principal_000001   # 201, then 200
curl -s "http://127.0.0.1:8314/principals?q=pri"                                             # prefix search
curl -s http://127.0.0.1:8314/principals/principal_000001                                    # with its groups
curl -s -X POST http://127.0.0.1:8314/principals/principal_000001/disable                    # the only removal

# The week, in the person's own zone; then who in a group is working right now
curl -s -X PATCH http://127.0.0.1:8314/principals/principal_000001 -H 'Content-Type: application/json' \
  -d '{"availability":{"tzid":"Europe/Amsterdam","days":["MO","TU","WE","TH","FR"],"start":"09:00","end":"17:00"}}'
curl -s -X POST http://127.0.0.1:8314/principals/principal_000001/time-off -H 'Content-Type: application/json' \
  -d '{"starts_at":1790000000,"ends_at":1790600000,"note":"holiday"}'
curl -s "http://127.0.0.1:8314/principals/principal_000001/availability"                     # {available, reason}
curl -s "http://127.0.0.1:8314/principals/principal_000002/members?available_at="            # the assignment-pool read
```

`scripts/seed-sample-principals.sh` (installed as `~/bin/seed-sample-principals`)
creates the operator, the four Northwind demo humans already named in kanban's
`card_events.actor`, and the `Data Team` and `Security` groups. It is idempotent
and refuses to guess if two rows ever share a name.

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
caller is an assignee picker reading keystrokes. Anything that already carries
FTS5 syntax — a quoted phrase, `*`, a column prefix — is passed through as
written, which is what the seed script's exact-name lookup relies on.

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
