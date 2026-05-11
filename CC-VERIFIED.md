# Claude Code — Verified Behaviors

Empirical findings from testing `claude_code_version: 2.1.138` on 2026-05-09 / 2026-05-10. Cited from elsewhere in the design set; this doc is the consolidated reference.

## Subagent injection

| Mechanism | Result | Notes |
|---|---|---|
| `--agents '<inline-json>'` | ✅ Works in `--input-format stream-json` | Subagent appears in init event's `agents` list. Dispatchable via `Task`/`Agent` tool with `subagent_type: "<key>"`. Defaults to Haiku 4.5 regardless of `model` field in JSON (probably not honored, not confirmed). |
| `--agents @<file>` | ❌ Silently ignored | Loads zero custom agents. |
| `--agents <bare-path>` | ❌ Silently ignored | Same. |
| `~/.claude/agents/<slug>.md` (filesystem) | Not tested but documented as auto-discovered | Available as a fallback if `--agents` argv size becomes a problem. |

**Verified `--agents` JSON shape:** `{"<slug>": {"description": "...", "prompt": "...", "tools": ["Read", "Bash"]}}`. Fields confirmed working: `description`, `prompt`, `tools`. `model` accepted without error but didn't appear to override default.

## System prompt

| Flag | CLAUDE.md auto-load | CC built-in identity | Sub auth | Notes |
|---|---|---|---|---|
| `--system-prompt <text>` | ⚠️ **Still loads** | Replaced | ✅ | Only replaces CC's built-in default. Project + global CLAUDE.md still attaches. |
| `--append-system-prompt <text>` | ✅ Loads | Kept | ✅ | Recommended default for our injection. |
| `--bare` | ❌ Suppressed | Suppressed | ❌ Forces `ANTHROPIC_API_KEY` | Not usable on the subscription path. |

**Implication:** there is no flag combination that gives full prefix isolation while staying on subscription auth. Treat the auto-loaded CLAUDE.md as part of the cache prefix CC owns. Our `BundleHash` represents only what we inject, not what CC composes.

## Permission settings

| Flag | Result |
|---|---|
| `--settings <path>` | ✅ Accepts a JSON file. `permissionMode` propagates to init event. |
| `--permission-mode <mode>` | ✅ One of `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan` |

Per-agent permission config viable: materialize a per-agent `settings.json` via `EnsureAgent`, register as tracked_file with scope=`settings`, pass via `--settings <path>` at session spawn.

## Tool subset

`--allowed-tools "<comma-or-space-sep>"` and `--disallowed-tools "<comma-or-space-sep>"` are documented and accepted. `--tools <tools...>` exists for replacing the entire built-in tool list (`""` for none, `"default"` for all, or specific names).

Native CC tools observed in init event (2.1.138): `Task, AskUserQuestion, Bash, CronCreate, CronDelete, CronList, Edit, EnterPlanMode, EnterWorktree, ExitPlanMode, ExitWorktree, Glob, Grep, Monitor, NotebookEdit, PushNotification, Read, RemoteTrigger, ScheduleWakeup, Skill, TaskOutput, TaskStop, TodoWrite, ToolSearch, WebFetch, WebSearch, Write`, plus MCP tools (Gmail, Calendar, Drive when configured).

## Init event surface

When CC starts in stream-json mode, the first event is `system/init` with all of: `tools`, `agents`, `skills`, `slash_commands`, `mcp_servers`, `permissionMode`, `model`, `apiKeySource`, `claude_code_version`, `memory_paths`. Useful for client-side discovery and for asserting what the session ended up with.

## Caching

In a session with no custom agents and no special flags:

- CC's built-in system prompt + tool definitions cache as a stable prefix.
- Global `~/.claude/CLAUDE.md` and project `CLAUDE.md` both load and become part of that prefix.
- Subagent invocations get their own cache keys (different prompt prefix), confirmed: subagent's `cache_creation_input_tokens` and `cache_read_input_tokens` populated independently.
- Verbose mode shows cache stats per turn — useful for debugging.

## PreToolUse hook — updatedInput

Verified 2026-05-11 (CC 2.1.138). The `hookSpecificOutput` response from a
PreToolUse hook accepts an `updatedInput` field alongside
`permissionDecision: "allow"`. CC merges that map into the tool input
before `tool.call()` runs.

```json
{
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "allow",
    "permissionDecisionReason": "user picked Red",
    "updatedInput": { "questions": [...], "answers": {"Q?": "Red"} }
  }
}
```

**AskUserQuestion contract** (`Oz` in the binary, ElH tool spec): its
`inputSchema` already includes optional `answers: record(string, string)`,
and `tool.call({questions, answers}) → {data: {questions, answers}}` —
the tool *reads* answers from input rather than prompting. Combined with
its own `checkPermissions` returning `behavior: "ask"`, this means a
PreToolUse hook that resolves to `allow + updatedInput: {questions,
answers}` short-circuits the interactive prompt entirely. Live contract
test on `br_1778535286766174715` (2026-05-11): tool result returned
"Got it: Red" from the agent in the same turn, no CLI prompt.

This is how the bridge-ui AskUserQuestion banner card delivers human
selections — the parked-ask `resolve` endpoint carries `updated_input`,
the prehook handler forwards it inside `hookSpecificOutput`, and CC's
AskUserQuestion call returns those answers as the tool result.

## Things to verify when needed (non-blocking)

- Whether `--agents` `model` field is actually honored (test by spawning a subagent that introspects).
- Argv length limit for `--agents` JSON in practice — currently no `@file` fallback.
- Whether `--allowed-tools` can scope individual MCP server tools (e.g. `mcp__serverName__toolName`) in addition to native ones.
- Slash commands available list — verified that all installed skills surface as `/<skill-name>` slash commands; per-agent filtering via `--disable-slash-commands` (nuclear) or potentially via settings.
