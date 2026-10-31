# About principal-store

## What it owns

`:8314`. The registry of *principals*: the things a permission is granted to and a card is assigned to. It gives every login, group and outside contact on a ticket one id (`principal_000001`, registered in kanban-store's entity-type registry) for other stores to join on. **It owns who may log in, hold a grant, be assigned a card or file a ticket — not "person".** The operator decided on 2026-10-01 that **people-store (`127.0.0.1:8321`, `person_000001`) owns the people in the operator's life**, with their chat handles, emails and relationships; a person there may point at a principal with `principal_id`. Never create a principal to stand for a chat contact: for a few hours on 2026-10-01 multichat made 84 `contact` principals that way, and they were disabled once their links moved to people-store. **What a principal may use is not here — it is in grant-store.** Routes are rooted at `/`. `CONTRACT.md` is the route table.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Principals, kinds and groups

`principals` carries `kind`, `display_name`, `email` (not unique), `disabled_at` and `is_administrator`. The kinds are served by `GET /kinds` — `human`, `group`, `contact`, `peer`, `service` — and callers read them from there. What a kind may do is served too, at `GET /kind-definitions` and as four computed booleans on every record: `acts_in_this_deployment` (may hold grants), `acts_as_caller` (a request may act as it, so it may join a group and be an administrator), `restricted_to_its_grants` (holding no grant of a relation refuses it) and `holds_service_principal_tokens` (it may be issued a token of its own in place of a login). **Other services read those booleans and never compare `kind` to a word.** A `peer` is another llm-bridge-server linked to this deployment: it acts, holds grants and joins groups like a human, but is restricted to its grants and has no availability (design note `d107154a-49ac-471b-a1b7-fc7975de37f3`). A `service` is a program on this host that calls other services as itself (orchestratord first): it acts through a service principal token llm-bridge-server issues it, never a login, is restricted to its grants, has no availability, and is the only kind with `holds_service_principal_tokens` (design note `f28e426c-e558-4330-9d43-39b4d0484d6a`). `group_members` holds membership — a member is a human, a peer or a service; **groups do not nest**. `GET /principals/{id}/members` lists a group's members and `GET /principals/{id}/groups` a principal's groups; `PUT` and `DELETE /principals/{group}/members/{member}` change them. `GET /principals?q=` is prefix search.

## No hard delete

A principal is never deleted: `POST /principals/{id}/disable` stamps `disabled_at` and `/enable` clears it. **A disabled row still resolves by id**, so a card, grant or ticket that points at it keeps working. `time_off` is the one table whose rows are hard-deleted.

## Contacts

A `contact` is someone outside this deployment that work arrives from — the requester on a ticket. A contact never logs in, never holds a grant, never joins a group and is never an administrator; `ActsInThisDeployment(kind)` is the check, and grant-store and the login path both ask it. `POST /contacts/resolve {"email":…,"display_name":…}` turns an address into one id: **201** with `created:true` the first time the address is seen, **200** with `created:false` after. It matches the address lowercased and trimmed, among active contacts only — an employee who also writes in as a customer is deliberately two rows. An existing contact **keeps the name it has**; overwriting it would rewrite the requester shown on every one of that person's tickets. ⚠️ This is the only write path here that finds a principal by anything but its id. That is its purpose; do not add a second.

## The administrator

A human, a peer or a service may carry `is_administrator`. kanban-store, grant-store and llm-bridge-server read it and let an administrator past every per-resource check they make. This store only records it: read it on any principal, set it with `PATCH /principals/{id} {"is_administrator":true}` (a patch that does not mention it leaves it alone). A **group** or a **contact** is refused it with a 400. A disabled principal is refused by those services before the flag is read, so disabling an administrator removes the access.

## Identity bindings

`identity_bindings` (`identity_binding_000001`) links a sign-in identity — `(provider, tenant_id, subject)`: for Entra the `tid` and `oid` GUIDs, for Discord an empty tenant and the Discord user id — to one active human. An administrator links it through llm-bridge-server, which fills `linked_by` from the caller; the login resolves it with `GET /identity-bindings/resolve` and treats a 404 as unprovisioned. **Never match a login on email or display name**, and never write a binding from the login path. One active binding per identity (a partial unique index); unlink needs a reason and keeps the row. `CONTRACT.md` has the routes.

## Availability

A human may declare an `availability` — the working week in their own zone, `{tzid, days, start, end}`, the same shape as a kanban board's `business_hours` — plus `time_off` rows (`timeoff_000001`, half-open epoch ranges). `GET /principals/{id}/availability?at=` reduces both to `{available, reason}`; the reasons are served at `GET /availability-reasons` and checked in this order: `disabled` → `no_schedule` → `time_off` → `off_hours` → `in_hours`. `?available_at=` on `GET /principals` and on a group's `/members` applies the same test to a list, and pages **after** the filter. **No schedule is unknown, and unknown is never available; there is no default zone and no measured presence.** Built for kanban-store's assignment pool (kanban card `31d1d990-1c41-4725-bdd5-bd2fe926ba7b`).

# Access and operations

## Who may call it

**No authentication, so it binds 127.0.0.1 only** (`PRINCIPAL_STORE_ADDR=127.0.0.1:8314` in the unit) — unlike most of its siblings, which listen on every interface. Do not widen the bind address without adding auth first. dash proxies it at `/api/principals/*` (`dash/server/api_principals.go`), behind dash's login. Unit `principal-store.service`, binary `~/bin/principal-store`. `~/bin/seed-sample-principals` (source `scripts/seed-sample-principals.sh`) seeds sample people and groups.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/principal-store`, root package `principalstore`, server in `cmd/principal-store`. SQLite at `~/.config/principal-store/principal-store.db`. The two variables it reads, `PRINCIPAL_STORE_ADDR` and `PRINCIPAL_STORE_DATA_DIR`, are declared once in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`, which makes `deploy-gate` check that tree too) and served read-only at `GET /settings`. A new variable is declared there or `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails, and a set `PRINCIPAL_STORE_` variable that nobody declared stops the service at boot; `deploy.sh` checks the running service's environment for one before it stops it. Declare nothing `Editable` while the service has no operator gate. ⚠️ **`go test` and `go build` need `-tags sqlite_fts5`**, or every test fails on `no such module: fts5`; the `Makefile` and `deploy.sh` set it. `schema.sql` still carries `DROP TABLE IF EXISTS principal_resources`: that table held a "works with" list that now lives in grant-store as the `works_with` relation, and its old routes answer a plain 404.

## Generated TypeScript types

The wire types are rendered to TypeScript, not copied: `./generate-ts.sh` runs tygo over the root package (`tygo.yaml` excludes `store.go`) and writes `ts/model.ts`, published source-only as `@kayushkin/principal-store-types` (`file:../principal-store/ts`). A type must live outside `store.go` to be rendered — `Principal`, `Patch` and `Counts` are in `principal.go`, and the membership answer is the named type `GroupMembership` so its route has a type. bridge-ui consumes the package through this main clone, so a missing `ts/package.json` breaks `tsc` there.
