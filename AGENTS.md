# About grant-store

## What it owns

`:8315`. **Who may use what**: one table of `(principal_id, relation, resource_type, resource_id)` tuples, ids `grant_000001`. It exists because that question had no home — principal-store is a directory, permission-store's rules have no subject, and tool-store's per-instance opt-ins say where a tool is wired, not who may have it. This store records grants and answers reads; **the services that own a resource enforce them**. Routes are rooted at `/`. `CONTRACT.md` is the route table.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Relations are a served vocabulary

`GET /relations` serves each relation with the resource types it applies to, whether it is enforced, whether it accepts `*`, and a description; `GET /resource-types` serves the types (`agent`, `instance`, `machine`, `skill`, `tool`, `board`, `operation_type`, `principal`, `peer_connection`, `reference_type`, `reference`). Callers and UIs read them from there and never restate them. Today: `can_use` (tool, skill), `can_run_as` (agent) and `can_dispatch_on` (instance, machine) are enforced by llm-bridge-server at session start; `can_run_operation` (operation_type, whose id is the type name, e.g. `classification.run`) is enforced by llm-bridge-server at `POST /operations`, lenient or strict by its `operations.grant_enforcement` setting; `can_view`, `can_edit` and `can_administer` (board) are enforced by kanban-store, and each includes the one before it; `can_start_sessions_as` (principal, no `*`: a service such as orchestratord may start sessions that run as that principal, and drive them) and `can_call_through` (peer_connection: may list that connection and call through it) are enforced by llm-bridge-server; `works_with` (agent, instance, machine, skill, tool) is **advisory** — bridge-ui's kanban "Runs on" picker reads it to put an assignee's instances first, and nothing refuses anything because of it. Adding a relation means adding it in `relation.go` so the route serves it.

## A grant on every resource: `*`

A grant whose `resource_id` is `*` names every resource of its type, including ones created later. Only a relation with `accepts_all_resources` takes it — `can_call_through`, `can_see_references` and the five below: `can_use`, `can_run_as`, `can_dispatch_on`, `can_run_operation`, `can_observe_sessions_on` — and no owner is asked, because `*` names no row. A board grant of `*` is a 400 until kanban-store reads it, so it is never stored and silently ignored; `works_with` refuses it because a list of everything advises nothing. Added 2026-10-20 for linked bridge servers (design note `d107154a-49ac-471b-a1b7-fc7975de37f3`), so a role such as "may dispatch on any instance" survives adding an instance.

## Both owners are asked before a write

`POST /grants` checks the principal with principal-store, reading its `acts_in_this_deployment` answer rather than its kind (a human, a group, a peer or a service holds grants; a `contact` does not) and the resource with the store that owns it — llm-bridge-server for agents, instances, machines, peer connections and operation types (its open `GET /operation-types`), principal-store for a `principal` resource, skill-store, tool-store, kanban-store for boards (`resource_checker.go`). **400 if either says no, 502 if either cannot answer, and nothing is written.** It is idempotent on the active tuple: 201 the first time, 200 with the stored row after. **`POST /grants/{id}/revoke` is the only removal**; it is idempotent and never deletes the row.

## The effective read

`GET /principals/{id}/effective` is what an enforcing service reads: the principal's own active grants ∪ those of every active group it is in, with the groups read **live from principal-store on each call**. A principal it does not know is 404, and a principal-store that is down is **502, never `[]`** — an empty answer would read as "holds nothing", which the enforcing services treat as "not restricted".

## What the enforcing services do with a grant

The shape is lenient by the operator's choice: **a principal holding no grant of a relation is not restricted by it** — unless principal-store marks its kind `restricted_to_its_grants`, as it does a `peer`, which holding no grant refuses — so a default principal set before any grant exists locks nothing out; holding one grant of a relation restricts the principal to what its grants of that relation name. An administrator (principal-store's `is_administrator`) is past every check. A session with no principal never touches this store. llm-bridge-server's own prompt states the session-start rule in full (`internal/server/grant_gate.go`, `tool_provision.go`), and kanban-store's states the board rule. **`can_see_references` is the exception, and strict**: reference-store (which owns `reference_type`, a type name or `*`, and `reference`, one record's id as written in prose) shows a record of a grant-decided type only to an administrator or a holder of the type, of `*`, or of that record, so a principal holding none sees no such chip. Not enforced anywhere: `can_use` on a **skill** is stored but nothing reads it, because Claude Code always lists `~/.claude/skills` and the only way to hide it (`CLAUDE_CONFIG_DIR`) also relocates `.credentials.json`.

# Access and operations

## Who may call it

There is no off switch: every route except `/health`, `/relations` and `/resource-types` needs one of two headers, and neither is a **401** (`principal_enforcement.go`). `X-Grant-Store-Service-Token` is for internal services — llm-bridge-server reading a session's grants at spawn, kanban-store reading board grants, an operator's script — and is unrestricted. `X-Principal-Id` is set by a gateway from a verified login or a peer token and trusted as sent; it must name an active principal that principal-store says `acts_as_caller` (a human, a peer or a service). An administrator is unrestricted. Any other principal may: create or revoke a **board** grant only on a board it holds `can_administer` on, directly or through a group (handing a board over is part of administering it); read a grant it holds, or any grant on a board it administers; list grants only with `principal_id` set to itself or with `resource_type=board` and a board it administers; read `/principals/{id}/effective` only for itself. Every other resource type is the operator's, through the service token. **A grant a principal may not read is 404**, the same as one that does not exist.

## Units, addresses and tokens

Binds `127.0.0.1:8315` (`GRANT_STORE_ADDR`). The unit names its owners by URL — `PRINCIPAL_STORE_URL` (also the owner of `principal` resources), `LLM_BRIDGE_URL` (asked with `LLMBRIDGE_SERVICE_TOKEN`, since its instance, machine and peer connection routes are operator routes; unset, each such check is a 502), `SKILL_STORE_URL`, `TOOL_STORE_URL`, and `KANBAN_STORE_URL` for boards. Tokens come from the host-local `~/.config/principal-gating-tokens.env` (mode 600), loaded by the drop-in `grant-store.service.d/principal-gating.conf` — **never put a token in the unit file tracked in this repo**. Without kanban-store's service token a board check is a 401 from kanban-store, reported as the owner being unable to answer (502). Reached three ways: directly with the service token; through llm-bridge-server's `/grant-store/…` proxy, which sets `X-Principal-Id` from a verified login or a session's principal token; and through dash's `/api/grants/*` (`dash/server/api_grants.go`), which adds dash's store credentials. bridge-ui draws a **Grants** tab and a Grants section on each principal, gated on `grantStoreBasePath`.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/grant-store`, root package `grantstore`, server in `cmd/grant-store`. SQLite at `~/.config/grant-store/grant-store.db` (WAL, foreign keys on, `_txlock=immediate` so every transaction takes the write lock at `BEGIN`). No FTS build tag is needed, unlike its siblings. The ten variables it reads are declared once in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`, which makes `deploy-gate` check that tree too) and served read-only at `GET /settings`, to the service token or an administrator only. No code here calls `os.Getenv`: a new variable is declared there and read from the registry, or `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails. A set `GRANT_STORE_` variable that nobody declared stops the service at boot, and so does a missing `GRANT_STORE_SERVICE_TOKEN`; `deploy.sh` checks the running service's environment for both before it stops it. Keep a secret a `string` setting, so no parse error can quote it. `deploy.sh`'s smoke check presents the service token and proves that a bare call answers 401. Pushes to `github.com/kayushkin/grant-store` (public).

## Generated TypeScript types

`./generate-ts.sh` runs tygo and writes the wire types to `ts/` as `@kayushkin/grant-store-types`. `tygo.yaml` excludes `store.go`, `principal_directory.go`, `principal_enforcement.go`, `resource_checker.go` and `settings.go`, so a type must live elsewhere to be rendered — `Grant` and `Counts` are in `grant.go`.
