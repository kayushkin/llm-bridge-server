# Model-Facing CLI Surface

Most non-native capabilities reach the model as CLI commands described in the appended system prompt and invoked via the harness's Bash-equivalent. This doc covers what those commands look like, where they live, and which capability each one fronts.

Companion to `TOOL-ROUTING.md` (the routing rule) and `HARNESS-LAYER.md` (which is responsible for emitting the descriptions).

## Why CLI, not MCP

`TOOL-ROUTING.md` covers this in detail. Short version: when a capability has no native equivalent on the harness, MCP and CLI are both options. CLI wins because:

- Real `tool_use` (Bash) — fully trained, fully reliable.
- No daemon to manage or `--mcp-config` to write.
- Composable in shell (`bridge memory search "x" --json | jq`).
- Independently testable from a terminal.

MCP only wins when streaming partial tool output matters or when typed args are essential. Neither applies to the inber/store-shaped capabilities we're exposing.

## Capability → CLI command mapping

For each capability the model needs, exactly one CLI command. Each command supports `--json` for structured output (default human-readable).

| Capability | Canonical CLI command | Backed by |
|---|---|---|
| **Cross-harness agent dispatch** | `bridge agent ask <slug> "<task>"` | bridge-server session API |
| **Tool registry list** | `bridge tools list [--harness X]` | tool-store |
| **Tool registry invoke** | `bridge tools run <name> --args '<json>'` | tool-store (proxies to MCP/CLI/in-process impl) |
| **Memory search** | `bridge memory search "<query>"` | memory-store |
| **Memory save** | `bridge memory save "<text>"` (or `--from-stdin`) | memory-store |
| **Memory recent / show / forget** | `bridge memory recent / show <id> / forget <id>` | memory-store |
| **Notes / todos** | `bridge notes list / get <id> / write / done` | noteboard |
| **Bus publish** | `bridge bus publish <topic> "<payload>"` | bus |
| **Bus tail** | `bridge bus tail <topic> --since <t>` | bus |
| **Skills get full body** | `bridge skills get <name>` | skill-store (model uses this when header injection wasn't enough) |
| **Kanban blackboard** | `bridge kanban post / claim / list / done` | kanban-store (team coordination — see `TEAM-ORCHESTRATION.md` §6) |
| **Verification** | `bridge verify task <card> / smoke` | tool-store runner + conformance (ground-truth — see `TEAM-ORCHESTRATION.md` §20) |
| **Inber-runtime ops** (only for inber-hosted sessions) | `inber chat / run / btw / sessions / config` | inber's HTTP API |

Two CLI binaries, two scopes:

- **`bridge`** — the omnibus client for cross-harness, cross-store capabilities. Lives in `~/repos/llm-bridge-server/cmd/bridge-cli/` (or its own repo if it grows).
- **`inber`** — only for inber-runtime-specific concerns. Stays in `~/repos/inber-cli`.

This is the **decision** falling out of the chat: things that orchestrate across harnesses or hit a store directly use `bridge`. Things that are about inber-the-runtime use `inber`. The earlier framing of "inber CLI as universal tool surface" was a holdover from when inber was the orchestrator — corrected.

## Why one omnibus `bridge` binary instead of per-service CLIs

Considered: separate `tool-store`, `memory`, `note`, `bus` binaries. Decided against because:

- **Allowlist size** in CC's `.claude/settings.json`. One `Bash(bridge:*)` entry covers everything; per-service means N entries.
- **Discoverability.** Model runs `bridge --help` once, sees the full surface. With separate CLIs, model has to know each binary name.
- **Consistency.** Same binary, same flag conventions (`--json`, `--from-stdin`, `--input-file`, stderr format).
- **Single deployable.** One binary, one update path.

Subcommands route to the right service internally — `bridge memory search` hits memory-store on `:8160`-equivalent, `bridge tools run` hits tool-store on `:8302`, etc. Implementation is thin: each subcommand is a typed HTTP client call.

## What the renderer emits

For an agent enrolled with non-native tools/skills, the appended system prompt gets a section like:

```
## Available CLI tools

For non-native operations, use these CLI commands. All support `--json` for structured output;
use `<cmd> --help` for full options.

- `bridge memory search "<query>" --json` — semantic recall over agent memory
- `bridge memory save "<text>" --json` — persist a memory
- `bridge tools run <name> --args '<json>' --json` — invoke a tool from the tool-store registry
- `bridge agent ask <slug> "<task>" --json` — delegate to subagent <slug> (runs on harness <h>)
- `bridge notes list --tag <t> --json` — read from noteboard
```

Cross-harness delegation lines are added per cross-harness subagent the agent has — one line each, with the subagent's preferred harness called out. No tool registration, no MCP config, just description.

## Permission allowlist for CC

In `.claude/settings.json` (per-agent, materialized via `EnsureAgent`):

```json
{
  "permissions": {
    "allow": [
      "Bash(bridge:*)",
      "Bash(jq:*)",
      "Bash(rg:*)",
      "Bash(cat:*)",
      "Bash(ls:*)",
      "Bash(head:*)",
      "Bash(tail:*)"
    ],
    "deny": [
      "Bash(rm:*)",
      "Bash(sudo:*)",
      "Bash(curl:*)"
    ]
  }
}
```

`Bash(bridge:*)` is the only entry needed for the unified CLI. Add `Bash(inber:*)` only for agents that run on inber-the-runtime and want runtime-specific commands.

## Implementation order

This is a separate work track from the harness layer (P1-P7 in `IMPLEMENTATION-ROADMAP.md`). Sketch:

1. **bridge-cli skeleton** — Cobra-style CLI binary at `~/repos/llm-bridge-server/cmd/bridge-cli/`. Subcommand stubs for `agent`, `tools`, `memory`, `notes`, `bus`, `skills`. Each stub exits with "not implemented yet" but doesn't error on `--help`.
2. **bridge agent ask** — call bridge-server's session API, subscribe to SSE, capture final assistant text, print. Highest-leverage subcommand because it unblocks cross-harness dispatch.
3. **bridge tools list / run** — wraps tool-store HTTP API. Unblocks "tool-store registry callable from inside any harness."
4. **bridge memory / notes / bus** — wraps the respective stores. One PR per store.
5. **bridge skills get** — wraps skill-store. Lets the header-only injection model grab full SKILL.md bodies on demand.

## Why the inber-cli rescoping matters

Without this split, every "I want to do X" capability ends up as `inber X` and the inber-cli surface bloats with things that don't belong there. The boundary stays clear:

- inber-cli = inber-runtime client (`chat`, `run`, `sessions list` for inber-hosted sessions).
- bridge-cli = cross-cutting client for stores and bridge-server.

Both binaries can coexist on PATH; agents typically only allowlist one or the other depending on their workload.
