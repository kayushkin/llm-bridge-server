# permissions (formerly permission-store)

`internal/access/permissions`, package `permissions`. It was the standalone permission-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `permissions`. Its record ids did not change.

The rule engine for tool-call gating across the llm-bridge harnesses: rules, the verdict on one tool call, and an audit row for every verdict. A package of llm-bridge-server, `internal/access/permissions`, which that server mounts under `/permissions/` with the paths the standalone service on `127.0.0.1:8304` served (`GET /healthz`, `GET`/`POST /rules`, `GET`/`PATCH`/`DELETE /rules/{id}`, `POST /evaluate`, `GET /audit`). The server's Claude Code `PreToolUse` prehook evaluates every tool call with the same `Engine` the routes use, in process, so a rule written through the routes drops the compiled patterns the prehook reads. The `bridge_perm` MCP server this used to back was deleted from `llm-bridge-claudecode` on 2026-05-05 (the story is in `docs/cc-mcp-retrospective.md`).

## Status

The rules and the audit live in Postgres, in the module `permissions` (role `permissions_module`), built by `migrations/*.sql`. `0002_insert_the_default_rules.sql` inserts the eighteen default rules (`seed-v1-…`) once, when a module is first migrated: a default someone deletes stays deleted and one someone edits stays edited. `ImportStandaloneDatabase` copies a standalone store's SQLite file into a freshly migrated module: it removes the module's untouched defaults and copies the file's rules (its own defaults as they were edited since, and every rule people added) and every audit row, in the order they were written. Rule and audit ids keep their text forms (`seed-v1-…`, `rule_<nanoseconds>_<hex>`, `aud_<nanoseconds>_<hex>`) and times stay RFC 3339 text. `GET /healthz` answers `{"rules":N,"status":"ok"}`; the standalone store named its database file there.

The engine is `engine.go`, the HTTP surface `handlers.go`, the Bash splitter `splitter/`. bridge-ui's Permissions page (`BridgePermissions.tsx`) is the rules editor, audit browser and dry-run tester.

Still open: `audit.resolved_by` is declared and never written; `EvaluateResponse` has no `updated_input` although llm-bridge-server's client parses one; a rule has no subject — scope is `global`, `bridge:<session>` or `instance:<id>`, never a principal.

## Layout

```
migrations/       the module's schema and its default rules
splitter/         AST-based Bash command splitter (mvdan.cc/sh)
cmd/probe/        CLI: read a Bash command, dump its atoms as JSON
```

## splitter

Parses a Bash command line into a flat list of `Atom`s — one per discrete command invocation or write redirection — so a rule engine can evaluate each piece independently. Designed as a security boundary: anything the splitter fails to extract becomes invisible to the rule engine, so it errs toward over-reporting.

```go
import "github.com/kayushkin/llm-bridge-server/internal/access/permissions/splitter"

atoms := splitter.Split(`git status && bash -c "rm -rf $(pwd)"`)
// → 4 atoms: git status, bash -c <str>, rm -rf $(...), pwd
```

`AtomCall` covers every command invocation (including those nested in command/process substitutions, control flow, subshells, function bodies, and `bash -c` / `eval` strings). `AtomRedirect` covers write-side redirections (`>`, `>>`, `>|`, `&>`, `&>>`). `AtomUnparseable` is the catch-all for parser failures — rule engines treat this as `ask`, never silently allow.

## probe

```
go build ./cmd/probe
./probe 'git status && (cd /tmp && bash -c "rm -rf foo")'
```

Prints the atom list as indented JSON.

## Tests

The store's tests run on Postgres. From the repository root:

```
export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"
go test ./internal/access/permissions/...
go test ./internal/access/permissions/splitter -fuzz=FuzzSplit -fuzztime=30s
```
