# permission-store

Canonical rule engine for tool-call gating across the llm-bridge harness ecosystem. llm-bridge-server's Claude Code `PreToolUse` prehook (`internal/server/permission_prehook.go`) posts every tool call to `POST /evaluate` and turns the verdict into the hook's allow/deny/ask. The `bridge_perm` MCP server this used to back was deleted from `llm-bridge-claudecode` on 2026-05-05 (commit `c28fc84` there; the story is in `docs/cc-mcp-retrospective.md`).

## Status

**Running.** SQLite store with migrations (`store.go`), the cascade rule engine (`engine.go`), the HTTP surface (`internal/server/`: `/rules`, `/evaluate`, `/audit`, `/healthz`), 17 seed rules (`seed.go`), the Bash splitter (`splitter/`), a systemd unit and `deploy.sh`, and an e2e smoke (`scripts/e2e-smoke.sh`). bridge-ui's Permissions page (`BridgePermissions.tsx`) is the rules editor, audit browser and dry-run tester. For months this line said "step 1 of 7 — Bash splitter complete", which was false from commit `f7a9d1d` on.

Still open: `audit.resolved_by` is declared and never written; `EvaluateResponse` has no `updated_input` although llm-bridge-server's client parses one; a rule has no subject — scope is `global`, `bridge:<session>` or `instance:<id>`, never a principal. ⚠️ Until 2026-09-11 the live prehook sent the bridge id under the key `session_id` and never sent the instance, so no scoped rule had ever matched from a real session; fixed in llm-bridge-server that day.

## Layout

```
splitter/         AST-based Bash command splitter (mvdan.cc/sh)
cmd/probe/        CLI: read a Bash command, dump its atoms as JSON
```

## splitter

Parses a Bash command line into a flat list of `Atom`s — one per discrete command invocation or write redirection — so a rule engine can evaluate each piece independently. Designed as a security boundary: anything the splitter fails to extract becomes invisible to the rule engine, so it errs toward over-reporting.

```go
import "github.com/kayushkin/permission-store/splitter"

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

```
go test ./...
go test ./splitter -fuzz=FuzzSplit -fuzztime=30s
```
