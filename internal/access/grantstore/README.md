# grant-store

`127.0.0.1:8315`. The registry of *grants* — which principal (a human or a
group from principal-store) may use which tool or skill, run as which agent,
and start sessions on which harness instance or machine.

Part of the store family alongside principal-store `:8314`, prediction-store
`:8313` and quote-store `:8309`, and the same shape as all of them: one record
type, ids handed out here (`grant_000001`) and joined on everywhere else,
routes rooted at `/`, loopback bind with dash as the only door.

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

## Run it

```bash
./deploy.sh          # test, build, install the unit, start, smoke-check the API and the bind
make check           # fmt, vet, test, build
```

Env: `GRANT_STORE_ADDR` (default `127.0.0.1:8315`), `GRANT_STORE_DATA_DIR`
(default `~/.config/grant-store`), and the owners a grant is checked against:
`PRINCIPAL_STORE_URL` (`:8314`), `LLM_BRIDGE_URL` (`:8160`), `SKILL_STORE_URL`
(`:8301`), `TOOL_STORE_URL` (`:8302`). SQLite at `<data dir>/grant-store.db`.
Every variable the service reads is declared in `settings.go`, and `GET /settings`
describes them to the service token or an administrator; `CONTRACT.md` has the full list.

**The bind is loopback on purpose.** This service has no auth; dash is the front
door that adds it at `/api/grants`. `deploy.sh` fails if it finds the port
listening anywhere but `127.0.0.1`.

## Use it

```bash
# The Data Team may be offered Playwright (tool-store id 13)
curl -s -X POST http://127.0.0.1:8315/grants -H 'Content-Type: application/json' \
  -d '{"principal_id":"principal_000006","relation":"can_use","resource_type":"tool","resource_id":"13","note":"e2e"}'

# What a session started as Slava would be offered — his own grants plus his groups'
curl -s http://127.0.0.1:8315/principals/principal_000001/effective
curl -s "http://127.0.0.1:8315/principals/principal_000001/effective?relation=can_use&resource_type=tool"

# Who holds a grant on tool 13
curl -s "http://127.0.0.1:8315/grants?resource_type=tool&resource_id=13"

curl -s -X POST http://127.0.0.1:8315/grants/grant_000001/revoke     # the only removal
```

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
