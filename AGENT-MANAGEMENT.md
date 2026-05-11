# Agent Management — Definitions, Rendering, and `/agents` UI

## What this covers

How an agent's canonical definition (rows in agent-store) becomes the harness-side artifacts the runtime actually reads — `--agents <json>` for CC subagents (no file), `AGENTS.md` for Codex, equivalent for other harnesses. Plus the `/agents` UI scope and the rendering library shared between consumers.

Companion to `HARNESS-LAYER.md` (the `HarnessBridge` contract) and `TOOL-ROUTING.md` (per-harness native vs. inject decisions).

## Current state (verified 2026-05-09)

agent-store already has the core data model. Schema is largely correct, terminology is partially stale.

| Table | Role | Notes |
|---|---|---|
| `agents` | Canonical agent (slug, display_name, role, description) | ✓ |
| `orchestrators` | Per-runtime registry (`inber`, `openclaw`, `dash`) | Replace with "harness" naming. Keep as a name change, not a model change — `orchestrator_id` becomes `harness_id`. |
| `agent_orchestrators` | Per-harness agent config (system_prompt override, model, budgets, `subagent_allow`) | ✓ rename to `agent_harness` |
| `agent_tools` | Tool enrollment per (agent × harness) | ✓ rename to `agent_harness_tools` |
| `agent_nature` | Identity fragments by `kind` (identity, principle, value, user, project) | ✓ — these become the *body* of the rendered .md file |
| `agent_system_prompt_refs` | Cross-agent listings (e.g. AGENTS.md mentions another agent) | ✓ |
| `agent_name_aliases` | Per-context display names | ✓ |
| `file_distributions` + `file_scans` | Older drift mechanism — pre-tracked_files | Deprecate in favor of tracked_files; backfill if needed. |
| `tracked_files` + `tracked_file_versions` | Disk-is-truth versioned index | ✓ — this is where rendered subagent files get registered. |
| `machine_seed_*` | Per-runner seeding state | ✓ — used by remote runners. |

The renaming pass is mechanical — schema migrations + Go field renames — and clarifies that "orchestrator" and "harness" mean the same thing. Doing it before adding rendering keeps the new code from inheriting the old terminology.

## Canonical agent shape (post-rename)

```
agent {
  id                int
  slug              text  // url-safe; used in filename rendering
  display_name      text
  description       text  // for parent agents to know when to invoke this subagent
  role              text
  enabled           bool
  parent_agent_id   int?  // NEW: if non-null, this agent is a subagent under parent
}

agent_harness {                    // formerly agent_orchestrators
  agent_id, harness_id  PK
  system_prompt         text       // override; if empty, render from agent_nature
  model_primary, fallbacks
  budgets, limits
  subagent_allow        json       // ["*"] or [slug, slug, ...]
  is_default            bool
}

agent_harness_tools {              // formerly agent_tools
  agent_id, harness_id, tool_name
}

agent_nature {
  agent_id
  kind          text  // identity | principle | value | user | project
  content       text
  priority      int
  source_path   text  // optional: which input file this came from
}
```

Two real changes from current schema:

1. **`agent.parent_agent_id`** — makes the subagent relationship a proper FK. Today the relationship is implicit (everywhere except `agent_system_prompt_refs`). Explicit parent FK lets us answer "what subagents does agent X dispatch?" with a single query.
2. **Rename `orchestrators` → `harness`**, etc. Cosmetic but worth doing once.

## Rendering library

A new package, `~/repos/llm-bridge/render/`, owns harness-specific format. Imported by both agent-store (for preview / scheduled re-render) and each `llm-bridge-<harness>` (for write-on-EnsureAgent).

```go
package render

// Renderer turns a canonical agent definition into the bytes
// that go into the harness-specific file (or API field).
type Renderer interface {
    Harness() string
    AgentFile(agent AgentView) (RenderedFile, error)
    PreviewBundle(agent AgentView) (PreviewBundle, error)  // for /files preview
}

type AgentView struct {
    Agent       Agent
    Harness     HarnessConfig       // from agent_harness row
    Nature      []NatureFragment    // ordered by priority
    Tools       []ToolEnrollment
    Skills      []SkillEnrollment
    Subagents   []AgentView         // recursive — for parent agents
}

type RenderedFile struct {
    Path        string  // absolute path, e.g. ~/.claude/agents/<parent>__<sub>.md
    Body        []byte
    SHA256      string
    Mode        os.FileMode
}

// Registered renderers — one per harness.
var Registry = map[string]Renderer{
    "claudecode": &claudeCodeRenderer{},
    "codex":      &codexRenderer{},
    "inber":      &inberRenderer{},   // probably no file output; identity stays in DB
    // scaffolds...
}
```

Renderers are pure functions of the AgentView. Same input → same bytes. No filesystem side effects from the renderer itself; the caller writes.

### CC subagent rendering — verified 2026-05-09

**No file materialization.** CC's `--agents <json>` flag accepts inline subagent definitions at session spawn. Verified working in `--input-format stream-json` (the mode `llm-bridge-claudecode` uses); the subagent appears in the session init event's `agents` list and is dispatchable via the `Task`/`Agent` tool with `subagent_type: "<slug>"`.

The CC renderer outputs a **JSON map**, not a file:

```json
{
  "<slug>": {
    "description": "<for parent agent to know when to invoke>",
    "prompt": "<concatenated nature.content, ordered by priority>",
    "tools": ["Read", "Bash", "..."],
    "model": "haiku" | "sonnet" | "opus" | undefined
  }
}
```

The map key is the canonical agent name — there is no filename, so the filename-vs-frontmatter question is moot.

**Top-level agent identity** is delivered via `--append-system-prompt` (recommended; lets the user's `~/.claude/CLAUDE.md` and any project CLAUDE.md still load with project conventions) or `--system-prompt` (replace; for fully isolated sessions). Both verified working with subscription auth in non-`--bare` mode.

**`--bare` is incompatible with subscription auth** — it forces `ANTHROPIC_API_KEY`. Don't use it in the sub-billed path.

The CC renderer's outputs:

- For `EnsureAgent`: nothing. No per-agent filesystem state.
- For `PrepareSession`:
  - `--agents '<JSON map of all subagents reachable from this agent>'`
  - `--append-system-prompt '<assembled identity for the top-level agent>'`
  - `--allowed-tools '<comma-separated; per agent_harness_tools, native names per TOOL-ROUTING>'`
  - Optional `--model`, `--mcp-config`, etc. as needed

### Codex rendering

Codex doesn't have a native subagent mechanism. For top-level identity, the renderer composes the system_prompt-equivalent for `codex app-server`'s initialize call (and may emit an `AGENTS.md` for project conventions). For subagents, the renderer appends `bridge agent ask <slug>` CLI descriptions to the system prompt — model invokes via Codex's native `shell` tool. No tool registration step; same CLI-in-prompt pattern as other non-native capabilities.

### Inber rendering

Returns no file. The inber runtime reads agent-store directly. Renderer is essentially a passthrough that returns the assembled identity for inclusion in the API system_prompt field, used at session start.

## CRUD flows

### Create / edit / delete agent — much simpler than the file-based model

Because CC subagents are now per-session injection, agent CRUD is purely an agent-store concern. No filesystem propagation step.

```
POST   /agents { slug, display_name, role, description, parent_agent_id? }
PATCH  /agents/{id}/nature/{nature_id} { content }
POST   /agents/{id}/harness/{harness_id} { system_prompt, model, ... }
POST   /agents/{id}/tools { harness_id, tools: [...] }
DELETE /agents/{id}                              # soft-delete (enabled=0)
```

No `EnsureAgent` step for CC subagents. Effects show up the next time a session is spawned for an agent that has this one as a (transitive) subagent — `PrepareSession` rebuilds the `--agents` JSON fresh each time.

### `/agents` is the editor; `/files` is the debug surface

Two distinct UIs with different purposes:

**`/agents`** — primary CRUD surface for agent-store-driven agents. Capabilities (decided 2026-05-10):

1. **Edit the agent's identity / system prompt** as markdown. Backed by `agent_nature` rows — most edits land in a `kind=identity` row, structured edits (principle, value, etc.) get their own rows. Renderer concatenates by priority on read.
2. **Pick visible skills** — a multi-select against the skill-store catalog. Selected skills' *names + descriptions* land in the rendered identity ("Skills available to you: X — does ABC; Y — does DEF"). Full SKILL.md bodies are not loaded; the model is expected to grab them on demand via `bridge skills get <name>` (see `CLI-SURFACE.md`) or whichever harness mechanism applies. This is the "header-only injection" pattern from `TOOL-ROUTING.md`.
3. **Pick available tools** — multi-select against tool-store. Each enrolled tool routes per `TOOL-ROUTING.md` (native > MCP > CLI-described). For CC, the native tool subset is rendered into `--allowed-tools`; MCP tools land in the per-agent MCP config; CLI tools get a "## Available CLI tools" section in the appended system prompt.
4. **Per-harness enrollment toggles** — which harnesses this agent runs on. Each row in `agent_harness` represents one enrollment; the UI lets the user add/remove rows.
5. **Subagent tree** — view and navigate the parent-child relationship via `agent.parent_agent_id`. Create child agents from the parent's view.

**`/files`** — debug / inspection surface. Read-mostly. Shows:

- The materialized files agent-store has written or scanned: `~/.claude/CLAUDE.md` (global), project CLAUDE.md, MCP configs, per-agent settings.json, skill-store-managed skill files, slash-commands.
- For each: scope, agent_slug, last seeded hash vs. on-disk hash (drift indicator), version history.
- Used to verify seeding worked correctly, debug drift, view what a runner has on disk.
- Edits are still allowed for power users (e.g. directly editing global CLAUDE.md), but the *expected* edit path for agent-store-driven content is `/agents`.

The two UIs share the underlying data: `/agents` writes structured rows; agent-store materializes files; `/files` shows the materialization. Round-trip from raw file edits still happens via the existing `scan-import` → `kind=override` nature flow, but it's the exception path, not the primary one.

### What still needs file-render and drift detection

Some artifacts still belong on disk because the harness expects them there:

- **Project CLAUDE.md / AGENTS.md** when bridge-server-managed agents need shared per-repo context. These are user-authored shared truth; not generated by agent-store.
- **Codex AGENTS.md for top-level identity** when the Codex harness has no equivalent of `--agents`. Renderer still emits a file; harness bridge writes it; tracked_files watches it.
- **MCP config files** (`~/.claude/mcp-servers/<agent_slug>.json`) when an agent has MCP-typed tool-store entries. Per-agent file, written by `EnsureAgent`, registered as tracked_file with scope=`mcp-config`.

For each of these, the existing tracked_files / `scan-import` / drift flow applies. Renderer + atomic write + version row.

## Where rendering decisions live

```
agent-store
  ├── agents schema, CRUD, nature, harness config
  ├── tracked_files / versions / drift detection
  ├── /files preview: calls render.Registry[harness].PreviewBundle
  └── fires `agent.changed` events (HTTP webhook or pub/sub) on mutations

llm-bridge/render  (NEW)
  ├── interface Renderer
  ├── claudeCodeRenderer
  ├── codexRenderer
  └── inberRenderer (and others, scaffolded)

llm-bridge-claudecode  (and other wrappers)
  └── HarnessBridge.EnsureAgent:
       - fetch AgentView from agent-store
       - call render.Registry["claudecode"].AgentFile(view)
       - atomic write to RenderedFile.Path
       - register/update tracked_file via agent-store API

llm-bridge-server
  └── on session.create:
       - fetch AgentView from agent-store
       - HarnessBridge.PrepareSession returns SpawnSpec
       - spawn harness wrapper with SpawnSpec.Args/Env, CWD = user repo
```

Three boundaries, each with its own concern:

- **agent-store** — what the agent IS.
- **render** — what the agent LOOKS LIKE per harness (pure function).
- **harness bridge** — when and where to put the rendered output.

## Migration sequence

This is mostly mechanical and can happen in one PR per step:

1. **Schema rename.** `orchestrators` → `harness`, `agent_orchestrators` → `agent_harness`, etc. Add `agent.parent_agent_id`. Backfill from `agent_system_prompt_refs` where possible.
2. **Create `llm-bridge/render/` package** with the interface and stub renderers (no behavior yet). Add to llm-bridge.
3. **Implement `claudeCodeRenderer`** mirroring whatever logic agent-store currently uses to write CLAUDE.md / agent .md files (audit `files.go`, `context.go`).
4. **Wire `EnsureAgent`** in `llm-bridge-claudecode` to call the renderer + write + register tracked_file. Existing direct-write code in agent-store gets replaced by calls into the renderer (reuses the same pure function).
5. **Implement `codexRenderer`** — composes top-level identity for `codex app-server` initialize. Appends `bridge agent ask <slug>` CLI lines for each subagent in the system prompt. No custom tool registration needed.
6. **Deprecate `file_distributions`** in favor of `tracked_files`. Migrate any historical rows that don't have tracked_files counterparts.
7. **`/files` preview tab** switches to calling `render.PreviewBundle` for the resolved view, replacing whatever ad-hoc preview logic exists today.

## Verified CC behavior (2026-05-09 against `claude_code_version: 2.1.138`)

- ✅ `--agents <inline-json>` works in stream-json mode. Subagent registered, dispatchable via `Task` tool with `subagent_type`.
- ❌ `--agents @<file>` and `--agents <path>` do **not** load the file. Both silently treat as malformed JSON, register zero custom agents. **No file fallback.** If argv length becomes an issue, mitigate by (a) filtering subagents per `agent_harness.subagent_allow` so only the dispatchable set is included, or (b) writing to `~/.claude/agents/<slug>.md` as a separate path (CC also auto-discovers these from disk).
- ⚠️ `--agents` field honoring partially confirmed: `description`, `prompt`, `tools` work. `model` field is accepted (no error) but didn't appear to override the default Haiku-for-subagents behavior in our test. Treat as "may not be honored" until verified.
- ❌ `--system-prompt` (replace) does **not** suppress auto-loaded CLAUDE.md. The CLAUDE.md still loaded and its instructions were obeyed in addition to the replaced system prompt. So the replace-vs-append choice is purely about CC's built-in identity prelude, not about CLAUDE.md.
- ✅ `--append-system-prompt` works alongside CLAUDE.md auto-load as expected.
- ✅ `--settings <path>` accepts a JSON file and propagates `permissionMode`. Per-session permission config is a real lever.
- ❌ `--bare` requires `ANTHROPIC_API_KEY`; not compatible with subscription auth.

**Practical implications:**

- Subscription-billed CC sessions cannot fully suppress auto-loaded CLAUDE.md (project + `~/.claude/CLAUDE.md`). Treat that content as part of the cache prefix that CC owns; our bundle hash represents only what we inject.
- Default to `--append-system-prompt` for top-level identity. Nothing to gain from `--system-prompt` (replace) given CLAUDE.md still loads — replace just removes CC's helpful default identity.
- For per-session permission config, `--settings <path>` is the lever. Materialize a per-agent settings JSON once via `EnsureAgent`, register as tracked_file, pass via flag at session spawn.

## Open questions

1. **`--agents` JSON argv size at scale.** Not currently bounded. Filtering by `subagent_allow` is the first mitigation; falling back to disk-based `~/.claude/agents/<slug>.md` is the second. Profile when it becomes relevant.
2. **`--agents` `model` field.** Accepted but possibly not honored. Test by spawning a subagent that emits its own model identifier (e.g. via a tool call that introspects the API response). Defer until needed.
3. **Codex AGENTS.md content.** With CLI-in-prompt for delegation, AGENTS.md (if rendered at all) is for project-level conventions, not subagent surface. Decide whether agent-store renders a per-agent AGENTS.md when wiring Codex, or whether AGENTS.md stays a user-authored file like project CLAUDE.md is for CC. Probably the latter.
4. **Inber's role.** Inber-runtime sessions don't need rendered files (reads agent-store directly). When a non-inber session delegates to an inber-runtime subagent (e.g. via `bridge agent ask <slug>`), bridge-server spawns a fresh inber session for it; inber reads agent-store directly. Normal agent-store consumption, no special render needed. Confirm.
5. **Project-scoped natures.** `agent_nature.kind=project` and the `project_nature` join — how do these flow into rendered output? Do they only appear when the agent is invoked *for that project*? Probably yes; renderer takes a `project` arg and filters. Out of scope for the first cut.

## What this enables

- One source of truth for agent definitions (agent-store), one rendering library (`llm-bridge/render`), one materialization mechanism (`HarnessBridge.EnsureAgent`).
- Adding a harness = implement `Renderer` + `HarnessBridge`, no agent-store changes.
- Adding a new agent = CRUD against agent-store, files appear automatically across all enrolled harnesses.
- Editing identity = edit the typed natures via UI, OR edit the raw rendered .md, both flow back; renderer is the single arbiter of what the harness ends up reading.
- Drift detection unified: tracked_files watches everything, sources distinguish ui-save / scan-import / runner-drift / harness-render.
