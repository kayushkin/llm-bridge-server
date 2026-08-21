# TODO: Wire up llm-bridge-jig harness

> **Status, measured against `main` on 2026-08-21: steps 1 and 2 shipped, step 3
> did not.** Steps 1 and 2 are kept below as a record of what was done; only step 3
> is still work.

`llm-bridge-jig` is now a functional harness binary. Three changes were needed:

## 1. ✅ Register jig in the binary-name table — DONE, and it moved repos

The function is no longer `BinaryName()` in `internal/harness/manager.go`. It is
`msg.HarnessBinaryName()` in the **llm-bridge** repo (`msg/provider.go`), so that
the server, the runner and the install scripts resolve one table instead of three.
It carries the jig case:

```go
case msg.HarnessJig:
    return "llm-bridge-jig"
```

`internal/harness/manager.go:445` is the caller today.

**Why it mattered:** without the case, `Available(msg.HarnessJig)` returned
`("", false)` and the server refused to spawn jig sessions.

## 2. ✅ Let jig past harness validation — DONE, and the allowlist is derived now

There is no hardcoded chain to add to. `handleCreateSession` calls
`isValidHarness(h)` (`internal/server/health.go:332`), which walks `allHarnesses` —
computed as `msg.AllHarnesses` minus the `disabledHarnesses` map. `msg.HarnessJig`
is in the canonical list and is not disabled, so it validates.

**Why it mattered:** POST /sessions with `"harness": "jig"` returned 400
"invalid harness". ⚠️ Note the shape of the fix, because it is the reason this
step cannot rot again: a hand-copied allowlist is what let `copilot_cli` drift out
of that file unnoticed, so the list is derived and never hand-maintained.

## 3. ⬜ Pass harness-specific start params (process.go) — STILL OPEN

`internal/harness/process.go` `StartParams` (line 57) doesn't include a `Profile` field. The jig harness expects `"profile": "<name>"` in its start params to know which YAML profile to load.

Options:
- **Minimal:** Add `Profile string` to `StartParams` — only jig uses it, others ignore it.
- **Extensible:** Add `Extensions map[string]json.RawMessage` to `StartParams` so any harness can receive custom start config without server-side struct changes. The session create request or agent-store config would populate these.

The extensible approach is cleaner long-term since other harnesses may need custom start params too (e.g., codex might need workspace config, aider might need git-diff mode).

**Why:** Without this, `llm-bridge-jig` starts with no profile — it spawns bare Claude Code with no profile-driven configuration, defeating the purpose of the jig harness.

⚠️ **This is less broken than it was, and the reason is worth knowing before you
pick the option.** `llm-bridge-jig` declares a `Profile` field with the JSON tag
`profile` on both its `StartParams` and its config params (`handler.go:35`,
`handler.go:58`), and
`handleConfig` can set the profile mid-session — which is the path model, effort and
max-budget already reach jig through, per `internal/server/health.go:218`. So a jig
session is configurable **after** it starts and not **at** the moment it starts.

## Source of profile at session creation

The caller (e.g., inber, dashboard, API consumer) needs a way to specify the profile when creating a session. This means `CreateSessionRequest` in `sessions.go` also needs a field (or extensions map) that flows through to the harness `StartParams`.
