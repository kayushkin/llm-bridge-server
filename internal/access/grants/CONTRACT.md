# grant-store — API contract

The registry of grants: which principal (a human, a group, a peer or a
service from principal-store) stands in which relation to which resource — an
agent, harness instance, machine, skill, tool, board and the rest of
`GET /resource-types`. "Who may use what" lives here and nowhere else.

Package `internal/access/grants` of llm-bridge-server, mounted by that
server under `/grants/`: every route below is served at
`/grants<route>`, with the same paths and answers the standalone service
on `127.0.0.1:8315` gave. The grants live in Postgres, in the module `grants`
(role `grants_module`), built by `migrations/*.sql`. All timestamps are epoch
seconds (`bigint`, 0 = unset).

The server decides who the caller is from its own credentials and hands the
verified caller to the handlers in the request's context
(`ContextWithCaller`); see [Who may read and change grants](#who-may-read-and-change-grants).
`ImportStandaloneDatabase` copies a standalone store's SQLite file into an
empty module, keeping every id, number, time and revoked row.

---

## The one table

### `grants`

| Field | Meaning |
|---|---|
| `id` | `grant_000001`. What a session's audit points at |
| `seq` | monotonic, assigned here; generates `id` |
| `principal_id` | a principal-store id of a kind principal-store says holds grants (`acts_in_this_deployment`): a human, a **group** or a **peer**. Checked with principal-store on write; **not a foreign key** — it is another database's id. A **contact** is refused: it is the outside party on a ticket, not someone this deployment grants anything to |
| `relation` | one of `GET /relations`; enforced in Go against `relation.go` |
| `resource_type` | one of `GET /resource-types`; enforced in Go against `resource_type.go`, and must be a type the relation allows |
| `resource_id` | the owner's id as text. Checked with the owner on write; not a foreign key. **`*`** names every resource of the type, now and later; only a relation with `accepts_all_resources` takes it (`can_use`, `can_run_as`, `can_dispatch_on`, `can_run_operation`, `can_observe_sessions_on`, `can_call_through`), and no owner is asked. `can_start_sessions_as` refuses it: every principal at once is every principal's access. A board grant of `*` is a 400 because kanban-store does not read it yet, and so is `works_with`, which is advisory |
| `note` | free text, trimmed: why, for whoever reads the list later |
| `granted_at` | |
| `revoked_at` | 0 = active. **The only removal**; see below |

A partial unique index on `(principal_id, relation, resource_type, resource_id)
WHERE revoked_at = 0` is what makes granting the same tuple twice idempotent
while still letting a revoked row and a fresh active row for the same tuple
coexist.

**There is no hard delete.** A grant is revoked, never removed, so a session
that was offered a tool can always point at the grant that offered it, and a
list with `include_revoked=true` is the full history.

**There is no display name.** The owner renames things, so a copied name goes
stale without anyone noticing; a UI resolves names live by id.

---

## The relation vocabulary

Served by `GET /relations`, never hardcoded by a caller:

| Relation | Applies to | Enforced | Meaning |
|---|---|---|---|
| `can_use` | tool, skill | yes | may be offered this tool or skill in a session started as this principal |
| `can_run_as` | agent | yes | may start a session as this agent |
| `can_dispatch_on` | instance, machine | yes | may start a session on this instance or machine |
| `can_view` | board | yes (kanban-store) | may see this kanban board and its cards |
| `can_edit` | board | yes (kanban-store) | may create, move, assign and annotate cards on this board; includes `can_view` |
| `can_administer` | board | yes (kanban-store) | may change the board's settings, columns, rules and triggers, or delete it; includes `can_edit` |
| `can_start_sessions_as` | principal | yes (llm-bridge-server) | may start a session that runs as this principal, and drive the sessions it started that way; the session holds that principal's grants, not the starter's. Given to a `service` principal such as orchestratord. No `*` |
| `can_call_through` | peer_connection | yes (llm-bridge-server) | may see this peer connection and call the connected server through it; read for a principal restricted to its grants |
| `works_with` | agent, instance, machine, skill, tool | **no** | advisory: what this principal usually works with. A card's dispatch picker reads it to put an assignee's instances first. Nothing refuses anything because of it |
| `can_read_mail` | mail_account | yes (mailstack) | may read this mail account's folders, threads and messages; holding none reads only the messages a ticket the principal may view links. No `*` |
| `can_import_mail` | mail_account | yes (mailstack) | may import into this mail account and change its messages; includes `can_read_mail`. No `*` |

**Enforced** means llm-bridge-server reads the relation at session start and
offers only what it names. As of 2026-09-11 it reads `can_use` for tools: a
session created with `principal_id` is offered the granted tools its instance
also has opted in (tool-store), provisioned by id; a principal with no such
grant gets the instance's opt-ins unchanged (lenient, the operator's choice);
`can_dispatch_on` and `can_run_as` refuse a session outside them at
create (403 `not_granted`) and at spawn, again leniently — a principal
holding none of a relation is not restricted by it; skills are stored but not
read yet. A grant pairing a relation with a type outside its
list is a 400 naming the allowed types. Nothing is normalized: `Can_Use` is a
400, not a rewrite.

The three board relations are stored as independent tuples. **kanban-store**,
not this store, decides that `can_administer` includes `can_edit` and `can_edit`
includes `can_view`, and it enforces them only when started with principal
enforcement on (see kanban-store's README). A kanban-store that enforces
answers a request with neither a principal nor its service token with 401, so
`KANBAN_STORE_SERVICE_TOKEN` must be set here for board grants to be written;
without it every board grant is a 502 quoting that 401.

`works_with` is the list principal-store used to keep as `principal_resources`
(2026-09-10 to 2026-09-11), moved here so there is one place to look.

---

## Routes

Errors are `{"error":"…"}`. Status mapping: not found → **404**; a malformed
or self-contradictory grant, or an id its owner says does not exist → **400**;
principal-store or a resource owner that could not be asked → **502** (nothing
is written); anything else → 500.

### `GET /health`

`{"status":"ok","counts":{"grants":N,"active":N,"revoked":N,"principals":N}}` —
`principals` is distinct principals holding an active grant.

### `GET /relations`

The table above as JSON: `[{"name","resource_types":[…],"enforced":bool,"accepts_all_resources":bool,"description"}]`,
in the order shown.

### `GET /resource-types`

`["agent","instance","machine","skill","tool","board","operation_type","principal","peer_connection","reference_type","reference","mail_account"]`.

### `GET /grants`

Query: `principal_id`, `relation`, `resource_type`, `resource_id`,
`include_revoked`, `limit`, `offset`. Every filter is optional and they
combine. Ordered by `resource_type, resource_id, relation, principal_id, seq`.
Bare array, `[]` when empty. An unknown `relation` or `resource_type` is a
**400** naming the vocabulary, never an empty answer, so a misspelling is not
read as "nobody has this".

`GET /grants?resource_type=tool&resource_id=13` is the reverse lookup: who
holds a grant on this tool.

### `POST /grants`

Body, decoded strictly (an unknown key is a 400):

```json
{"principal_id":"principal_000006","relation":"can_use","resource_type":"tool","resource_id":"13","note":"Playwright for e2e"}
```

Checks, in the order a caller can act on them: the relation, the resource type
and whether the relation allows it, the id shapes (`principal_…`; a numeric id
must be a plain decimal integer — a slug or a name is a 400 saying which id is
wanted), then **principal-store** is asked whether the principal exists, then
the **resource's owner** is asked whether the resource exists. Both owners are
asked before anything is written.

**201** with the grant when this call wrote it; **200** with the stored row
when an identical active tuple already exists (same `granted_at`, same
`note` — the repeat does not overwrite). A disabled principal and a disabled
resource are both accepted: the grant is dormant, and comes back with them.
Two identical creates at once write one row: the second meets the partial
unique index, writes nothing and answers 200 with the first's row.

### `GET /grants/{id}`

The grant, revoked or not. 404 if no such id; only the exact form names a
grant, so `grant_1` is 404.

### `POST /grants/{id}/revoke`

**200** with the grant. Idempotent: an already-revoked grant is returned with
its original `revoked_at`. 404 if no such id. Never asks an owner — a grant on
a resource that was deleted upstream is exactly the one most in need of
revoking, and an owner that is down must not pin a grant in place.

### `GET /principals/{id}/effective`

**The read llm-bridge-server makes at session start.** Every active grant the
principal holds, directly or through an active group it belongs to. Query:
`relation`, `resource_type` (optional filters, same vocabulary rules as
`GET /grants`).

Groups come **live from principal-store** on every call (`GET
/principals/{id}` there, which expands a human's active groups), so a
membership change shows at the next session start without this store being
told. A group's own effective set is its own rows: groups do not nest.

Each row is the grant as stored, so its `principal_id` says whether it is the
principal's own (equal to the id asked for) or inherited (a group's id). The
same resource granted both ways appears twice, because both are true, and
whoever is about to revoke the direct one needs to see the group still carries
it. Ordered by `resource_type, resource_id, relation`, the principal's own row
before inherited ones, then `principal_id`.

A principal principal-store does not know is **404**; a principal-store that
could not be asked is **502** — **never an empty list**, because an empty list
would start a session with nothing offered and no explanation.

---

## Owner checks

| `resource_type` | Owner asked | Id wanted |
|---|---|---|
| `agent` | the mounting server's agent-store, in its own process | agent-store's numeric `agents.id`. The slug is refused: agent-store lets it be renamed |
| `instance`, `machine` | the mounting server's harness-store, in its own process | harness-store's instance id (`inst-cc-local`) or machine id (`m_localhost`) |
| `peer_connection`, `operation_type` | the mounting server, in its own process | its peer connection id; the operation type's name |
| `principal` | principal-store, in the same process | `principal_000001` |
| `mail_account` | mailstack, in the same process | the account id `GET /mail/api/accounts` lists |
| `skill` | skill-store `GET /skills/{id}` | skill-store's numeric `skills.id` |
| `tool` | tool-store `GET /tools/{id}` | tool-store's numeric `tools.id` |
| `board` | kanban-store `GET /api/boards/{id}`, sending `X-Kanban-Store-Service-Token` when set | kanban-store's board id (a uuid) |
| `reference_type`, `reference` | reference-store `GET /types/{type}` and `POST /resolve`, sending `X-Reference-Store-Service-Token` | a type name; a record id as written in prose |

`HTTPResourceChecker` asks the owners that still run as services
(`ResourceTypesCheckedOverHTTP`), each call with a 3 s timeout; a 404 whose
body is Go's own `404 page not found` is reported as a misconfigured URL — a
502 naming the variable — not as a missing record. Asked about any other type
it answers an error saying the mounting server checks that one itself.

---

## Who may read and change grants

There is no off switch. Every route except `/health`, `/relations` and
`/resource-types` reads the caller from the request's context, where the
mounting server put it after verifying the request's own credential. No header
or body field a client sends names the caller: the standalone store's
`X-Grant-Store-Service-Token` and `X-Principal-Id` are gone. The caller is one
of:

- the **internal service** (`Caller{InternalService: true}`) — unrestricted.
  The server's own reads at spawn, and a request carrying the server's service
  token with no principal.
- a **principal** (`Caller{PrincipalID: …}`) — a principal the server verified.
  It must be active and of a kind principal-store says acts as a caller
  (`acts_as_caller`): a human, a peer or a service.

No caller, a malformed id, or an unknown, disabled, group or contact principal
is **401**. A principal-store that cannot be asked is **502**.

| Route | A principal may |
|---|---|
| `POST /grants` | grant on a **board** it holds `can_administer` on (directly or through a group); any other resource type is **403** |
| `POST /grants/{id}/revoke` | revoke a grant on a board it administers; **404** if it could not read the grant, **403** if it can read it but does not administer the board |
| `GET /grants/{id}` | read a grant it holds, or any grant on a board it administers; otherwise **404** |
| `GET /grants` | list with `principal_id` set to itself, or with `resource_type=board&resource_id=` a board it administers; otherwise **403** |
| `GET /principals/{id}/effective` | read its own; otherwise **403** |

A board administrator may grant `can_administer` to others: handing a board over
is part of administering it.

**An administrator** — principal-store's `is_administrator` on a human or peer — is
unrestricted here, like the internal service: reads and writes every grant on
every resource, whoever holds it. Administering the deployment includes handing
out and taking back access to it.
