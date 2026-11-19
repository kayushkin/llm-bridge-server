# hook-store

SQLite registry for bridge-managed harness hooks in the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Each row is a hook handler (harness, event, matcher, shell command) bound to a scope (`global`, `instance`, or `session`). The llm-bridge server queries this store at session spawn to decide which hooks to wire into the harness's native hook mechanism — for Claude Code, hooks are synthesized into a settings JSON passed via `--settings`.

The store is a pure data layer. It does not execute hooks, render settings JSON, or talk to harnesses — that belongs to the server.

## Usage

```go
import (
    "github.com/kayushkin/llm-bridge/msg"
    hookstore "github.com/kayushkin/hook-store"
)

s, err := hookstore.Open("~/.llm-bridge/hooks.db")
if err != nil { /* ... */ }
defer s.Close()

// Register a global PreToolUse hook for Claude Code that blocks `rm -rf /`.
s.CreateHook(&msg.Hook{
    ID:        "01HXYZ...",
    Harness:   msg.HarnessClaudeCode,
    Event:     "PreToolUse",
    Matcher:   "Bash",
    Command:   "/usr/local/bin/block-dangerous-rm",
    ScopeKind: msg.HookScopeGlobal,
    Enabled:   true,
})

// At session spawn, ask which hooks apply.
hooks, _ := s.ListApplicable(msg.HarnessClaudeCode, instanceID, sessionID)
```

## Scope resolution

`ListApplicable` returns enabled hooks across all three scopes with collisions resolved by specificity: when two hooks share `(harness, event, matcher)`, the most specific scope wins — `session` > `instance` > `global`. `instanceID` and `sessionID` may be empty for pre-instance / pre-session queries; those scope layers are skipped.

## Schema

Single `hooks` table with indexes on `(harness, event)`, `(scope_kind, scope_id)`, and `enabled`. WAL is enabled; foreign keys are on. Migrations live in `store.go`.

## Relationship to the ecosystem

This store is one component of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. When used with [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server), hook-store is loaded automatically and its endpoints are mounted on the server.
