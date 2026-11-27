# grants (formerly grant-store)

`internal/access/grants`, package `grants`. It was the standalone grant-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `grants`. Its record ids did not change.

The registry of *grants* — which principal (a human, a group, a peer or a
service from principal-store) may use which tool or skill, run as which agent,
start sessions on which harness instance or machine, and see or change which
board.

A package of llm-bridge-server, `internal/access/grants`, which that server
mounts under `/grants/` with the paths the standalone service on `:8315`
served. One record type, ids handed out here (`grant_000001`) and joined on
everywhere else, kept in the Postgres module `grants`.

## Why it exists

"Who may use what" had no home. principal-store is a directory; permission-store
matches tool-call patterns and has no subject — a rule says which tool, never
for whom; tool-store's per-instance opt-ins say where a tool is wired, not who
may have it. This store holds the tuples, and llm-bridge-server reads a
principal's effective set at session start and offers only what it names.

A grant is `(principal, relation, resource)`. Relations are a fixed list served
by `GET /relations`: `can_use` (tool, skill), `can_run_as` (agent),
`can_dispatch_on` (instance, machine) are **enforced** — llm-bridge-server
filters a session's offer by them; as of 2026-09-11 it reads `can_use` for
tools at spawn and refuses a session outside its `can_dispatch_on` /
`can_run_as` grants (a principal holding none of a relation is not
restricted by it); skills are not read yet — and `works_with` is advisory, the list
principal-store used to keep, moved here so there is one place to look.

## Use it

Through llm-bridge-server, as a principal or with its service token:

```bash
# The Data Team may be offered Playwright (tool-store id 13)
curl -s -X POST "$BRIDGE/grants/grants" -H "X-LLM-Bridge-Service-Token: $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"principal_id":"principal_000006","relation":"can_use","resource_type":"tool","resource_id":"13","note":"e2e"}'

# What a session started as principal_000001 would be offered — its own grants plus its groups'
curl -s "$BRIDGE/grants/principals/principal_000001/effective?relation=can_use&resource_type=tool" -H "X-LLM-Bridge-Service-Token: $TOKEN"

# Who holds a grant on tool 13
curl -s "$BRIDGE/grants/grants?resource_type=tool&resource_id=13" -H "X-LLM-Bridge-Service-Token: $TOKEN"
```

Tests run on Postgres: `export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"`
at the repository root, then `go test ./internal/access/grants/`.

`CONTRACT.md` is the route table and the field-by-field reference.

## Design notes

**Both owners are asked before anything is written.** principal-store confirms
the principal, and the resource's own store confirms the resource, so no row
ever names an id nobody hands out. A wrong id is a 400 naming the owner and the
id shape it wants; an owner that is down is a 502 and nothing is written.

**Groups are read live, not copied.** The effective set asks principal-store for
a human's groups on every call, so a membership change is visible at the next
session start without this store being told anything.

**There is no hard delete.** A grant is revoked, never removed, so a session's
audit can always point at the grant that offered it. Revoking is idempotent and
never asks an owner: a grant on a resource deleted upstream is the one most in
need of revoking.

**An unknown word is a 400, not an empty answer.** `GET /grants?relation=may_use`
names the vocabulary rather than answering `[]`, because `[]` reads as "nobody
has this", and `GET /principals/{id}/effective` answers 404 or 502 rather than
`[]` when principal-store cannot vouch for the principal, because `[]` would
start a session with nothing offered and no explanation.

## License

MIT — see [LICENSE](LICENSE).
