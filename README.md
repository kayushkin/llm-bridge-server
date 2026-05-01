# permission-store

Canonical rule engine for tool-call gating across the llm-bridge harness ecosystem. Backs the `bridge_perm` MCP server in `llm-bridge-claudecode` so every Claude Code tool call is evaluated against a single rule set.

## Status

**Step 1 of 7 — Bash splitter complete.** Service skeleton, MCP wiring, seed rules, settings toggle, rules editor UI, and ask-flow UI are still TODO. See `project_permission_prompt_followups.md` in the auto-memory for the full plan.

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
