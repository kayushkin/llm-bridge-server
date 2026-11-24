# About permission-store

## What it owns

`:8304`. The rule engine for tool-call gating across the llm-bridge harnesses: rules, the verdict on one tool call, and an audit row for every verdict. llm-bridge-server's `PreToolUse` prehook (`internal/server/permission_prehook.go` there) posts a tool call to `POST /evaluate` and turns the answer into the hook's allow, deny or ask. Routes: `GET`/`POST /rules`, `GET`/`PATCH`/`DELETE /rules/{id}`, `POST /evaluate`, `GET /audit`, `GET /healthz`. `README.md` has the status and the splitter's contract.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Rules and the cascade

A rule is `(scope, tool, RE2 pattern) → allow | deny | ask`, with a `priority`, a `message` and `enabled`. Scope is `global`, `bridge:<session id>` or `instance:<id>`. `Engine.cascade` orders the rules for a request most specific first — instance, then bridge, then global — and within a scope **lower priority wins**, ties broken by id. **A rule has no subject**: it says which tool, never for whom; who may use a tool is grant-store's question. When no rule matches, the verdict is `ask` with the sentinel rule id `default-ask`. Compiled patterns are cached in the engine, so every rule create, update and delete must call `InvalidateCache`.

## A Bash call is judged atom by atom

`splitter/` parses a Bash command line (mvdan.cc/sh) into a flat list of atoms — `AtomCall` for every command invocation, including those nested in substitutions, control flow, subshells, function bodies and `bash -c` or `eval` strings, and `AtomRedirect` for write-side redirections. **It is a security boundary: anything the splitter fails to extract is invisible to the rules, so it errs toward over-reporting.** The engine judges each atom: any `deny` denies the whole call at once; otherwise any `ask` makes it `ask`; an `AtomUnparseable` is `ask` with rule id `parse-fail`, never a silent allow. `cmd/probe` prints the atoms of a command as JSON.

## When the bridge asks, and when it does not

⚠️ **This store sees only the calls the bridge sends it.** The prehook reads the session's permission mode first: `block_all`, `plan`, `read` and `ask_all` are decided in the bridge without a rule consult, **`bypass` is allowed outright**, and only `ask`, `custom` and `auto` (for a tool not on its safe list) reach `POST /evaluate`. So a rule here does not bind a bypass session. Measured 2026-09-18: the audit table held 23,960 rows and the newest was from 2026-09-11, while the bridge logged a prehook call every few seconds — the sessions on this host run in bypass. A scoped rule also needs the bridge to send `bridge_id` and `instance_id`: only 1 of the newest 200 audit rows carries a `bridge_id`, and it is a probe, so **no scoped rule is known to have matched from a live session** — check `GET /audit` before trusting one.

## What is unfinished

`audit.resolved_by` is declared and never written. `EvaluateResponse` has no `updated_input`, although llm-bridge-server's client parses one. There is no principal scope. The `bridge_perm` MCP server this store once backed is gone from `llm-bridge-claudecode`; `docs/cc-mcp-retrospective.md` has the story and `docs/cc-pretooluse-plan.md` the plan that replaced it.

# Access and operations

## Who may call it

No authentication; it binds `127.0.0.1:8304` (`--port 8304` in the unit). Unit `permission-store.service`, binary `~/bin/permission-store`. It holds 50 rules today, all `global`: 19 seeded by `seed.go` (ids `seed-v1-…`), the rest written through the API — among them `multichat-send-message-ask`, which makes `mcp__multichat__send_message` an `ask`. bridge-ui's Permissions page (`BridgePermissions.tsx`) is the rules editor, the audit browser and a dry-run tester.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/permission-store`; engine and store in the root package, HTTP surface in `internal/server/`, server in `cmd/permission-store`. SQLite at `~/.config/permission-store/permission-store.db` (`--db` overrides), with migrations in `store.go`. `deploy.sh` builds, restarts and checks `GET /healthz`; `scripts/e2e-smoke.sh` is the end-to-end smoke. Pushes to `github.com/kayushkin/permission-store`.
