# TODO: Wire up llm-bridge-jig harness

`llm-bridge-jig` is now a functional harness binary, but the server doesn't know how to launch it yet. Three changes are needed:

## 1. Register jig in BinaryName (manager.go)

`internal/harness/manager.go` `BinaryName()` needs a case for `msg.HarnessJig`:

```go
case msg.HarnessJig:
    return "llm-bridge-jig"
```

**Why:** Without this, `Available(msg.HarnessJig)` returns `("", false)` and the server refuses to spawn jig sessions.

## 2. Add jig to harness validation allowlist (sessions.go)

`internal/server/sessions.go` `handleCreateSession` has a hardcoded check on line ~73:

```go
if h != msg.HarnessClaudeCode && h != msg.HarnessCodex && ...
```

Add `msg.HarnessJig` to this check.

**Why:** POST /sessions with `"harness": "jig"` currently returns 400 "invalid harness".

## 3. Pass harness-specific start params (process.go)

`internal/harness/process.go` `StartParams` (line 26) doesn't include a `Profile` field. The jig harness expects `"profile": "<name>"` in its start params to know which YAML profile to load.

Options:
- **Minimal:** Add `Profile string` to `StartParams` — only jig uses it, others ignore it.
- **Extensible:** Add `Extensions map[string]json.RawMessage` to `StartParams` so any harness can receive custom start config without server-side struct changes. The session create request or agent-store config would populate these.

The extensible approach is cleaner long-term since other harnesses may need custom start params too (e.g., codex might need workspace config, aider might need git-diff mode).

**Why:** Without this, `llm-bridge-jig` starts with no profile — it spawns bare Claude Code with no profile-driven configuration, defeating the purpose of the jig harness.

## Source of profile at session creation

The caller (e.g., inber, dashboard, API consumer) needs a way to specify the profile when creating a session. This means `CreateSessionRequest` in `sessions.go` also needs a field (or extensions map) that flows through to the harness `StartParams`.
