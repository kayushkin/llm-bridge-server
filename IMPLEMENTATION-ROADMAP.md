# Implementation Roadmap — Harness Layer + Agent Rendering

Translates the design docs (see README.md design index — `HARNESS-LAYER`, `TOOL-ROUTING`, `AGENT-MANAGEMENT`, `CLI-SURFACE`, `CACHE-RULES`, `CC-VERIFIED`, `CONTEXT-MIGRATION`) into ordered PRs across the affected repos.

## Dependency graph

```
                    [P1 agent-store rename]
                            │
              ┌─────────────┼─────────────┐
              ▼             ▼             ▼
       [P2 llm-bridge   [P5 inber-cli  [P9 assembly
        /render skel]    --json pass]   skeleton]
              │
       [P3 claudecode    
        renderer]        
              │           
       [P4 cc EnsureAgent
        + PrepareSession]
              │
       [P6 bridge-server 
        wires AgentReconciler]
              │
       [P7 BundleHash
        observability]
              │
       [P8 /agents UI/API]   ← separate, after P1
       
[P9 → P10..P15]  ← context-migration leaf-first ports, parallel track
[P16 codex catalog audit]  ← unblocks P17 codex renderer
[P17 codex renderer + bridge]  ← parallel after P2
```

Critical path is **P1 → P2 → P3 → P4 → P6**. Everything else either gates on those or runs parallel.

## Priority 1: Foundation (sequential, blocks most work)

### P1 — agent-store schema rename + parent FK

**Repo:** `~/repos/agent-store`

**Scope:**
- Migration: `orchestrators` → `harness`, `agent_orchestrators` → `agent_harness`, `agent_tools` → `agent_harness_tools`. Update all refs in Go code.
- Add `agents.parent_agent_id INTEGER REFERENCES agents(id) ON DELETE SET NULL`. Backfill nulls for existing rows.
- Add `agent_skills` table parallel to `agent_harness_tools` (per-agent skill enrollment, blocks skill routing in P3+).
- Update `tracked_files.scope` enum docs to include `mcp-config` value (no schema change; CHECK constraint or just convention).
- Bump `schema_embed.go` and update tests.

**Why first:** every other PR touches these tables. Doing the rename later means N rebases.

**Risk:** SQLite migration of column renames is non-trivial. Prefer ALTER TABLE RENAME COLUMN where supported; fall back to copy-table-rename pattern. Test on a snapshot of the prod DB before merging.

**Effort:** 1-2 days. Mostly mechanical.

---

## Priority 2: Render skeleton + first harness vertical

### P2 — `llm-bridge/render` skeleton

**Repo:** `~/repos/llm-bridge`

**Scope:**
- New package `render/`. Define `Renderer` interface, `AgentView`, `RenderedFile`, `Registry` map.
- Stub renderers per known harness: `claudeCodeRenderer`, `codexRenderer`, `inberRenderer`. All return `errors.New("not implemented")` initially.
- Define the `AgentReconciler` interface in `bridge/` (next to existing bridge interfaces). `EnsureAgent`, `PrepareSession`, `CleanupAgent` signatures from HARNESS-LAYER.md.

**Why now:** establishes the import contract. Concrete renderers can land independently after.

**Effort:** ~1 day.

### P3 — Claude Code renderer

**Repo:** `~/repos/llm-bridge` (extends P2)

**Scope:**
- Implement `claudeCodeRenderer.PreviewBundle` and the `--agents` JSON map builder.
- Pure functions of `AgentView`. Returns `{slug: {description, prompt, tools[]}}` JSON, identity string for `--append-system-prompt`, allowed-tools list, optional MCP config bytes.
- Tests with table-driven cases covering: top-level only, top-level with subagents, with skills enrollment header-only, with tool-store CLI tools.

**Effort:** 2-3 days. Most of the work is figuring out the right tool-name mapping (per `TOOL-ROUTING.md` capability table) and skill-summary extraction.

### P4 — `llm-bridge-claudecode` AgentReconciler implementation

**Repo:** `~/repos/llm-bridge-claudecode`

**Scope:**
- Implement `AgentReconciler` interface from P2.
- `EnsureAgent`: write per-agent MCP config (if any tool-store entries are MCP-typed) and per-agent settings.json (if any non-default permission rules). Register both as `tracked_files` entries via agent-store API.
- `PrepareSession`: call `render.Registry["claudecode"].PreviewBundle`, build `SpawnSpec` with all flags (`--agents`, `--append-system-prompt`, `--allowed-tools`, `--mcp-config`, `--settings`).
- `CleanupAgent`: remove the per-agent MCP/settings files, mark tracked_files missing.
- Wire into the existing `main.go` so the harness wrapper can fulfill these calls when invoked by bridge-server.

**Risk:** the existing `llm-bridge-claudecode` already has logic for spawning CC. Refactor carefully to add the new lifecycle hooks without breaking session handling. Probably worth a test pass against a live CC session before merging.

**Effort:** 3-5 days.

---

## Priority 3: Bridge-server orchestration

### P6 — bridge-server invokes AgentReconciler

**Repo:** `~/repos/llm-bridge-server`

**Scope:**
- On `agent.changed` event from agent-store: call `AgentReconciler[harness_id].EnsureAgent` for each registered harness.
- On `session.create`: call `AgentReconciler[harness_id].PrepareSession`, use returned `SpawnSpec` to fork the harness wrapper.
- Replace the existing scattered context-build logic in `internal/server/agents_context.go` with a call into `render.Registry`. (This is the first half of `CONTEXT-MIGRATION.md`'s step 3, but limited to the render path; the assembly extraction is separate.)
- Keep `harness_proxy.go` and `manager.go` mostly unchanged — only the prep step in front of spawn changes.

**Why this comes after P4:** there's no point wiring the orchestration if there's no harness implementing the contract. P4 gives one working harness; P6 lets it actually run end-to-end.

**Effort:** 3-4 days.

### P7 — BundleHash observability

**Repo:** `~/repos/llm-bridge-server` (extends P6)

**Scope:**
- Compute `BundleHash` (SHA256 of composed identity + sorted tools + agents JSON) at `PrepareSession`.
- Log `session.start` event with `bundle_hash`, persist on the session row.
- Emit `session.bundle.refresh` log event for any future mid-session re-materialization (rare, by policy).
- Surface bundle hash in `/sessions` UI so cache regressions are diagnosable.

**Effort:** 1 day.

---

## Priority 4: User-facing surfaces (parallel after P1)

### P5 — inber-cli runtime polish

**Repo:** `~/repos/inber-cli`

**Scope:** per `~/repos/inber/docs/cli-tool-surface.md` (post-rescope: inber-runtime only). `--json` on every retained subcommand, `--from-stdin`/`--input-file` for free-form-text inputs, standardized stderr format. The cross-harness commands previously planned here (`inber agent ask`, `inber bus`, etc.) moved to a separate `bridge-cli` track — see P5b.

**Why parallel:** doesn't depend on harness layer. Quality-of-life pass on inber's existing surface.

**Effort:** 2-3 days.

### P5b — bridge-cli skeleton + cross-cutting subcommands

**Repo:** `~/repos/llm-bridge-server/cmd/bridge-cli/` (new binary target)

**Scope:** per `CLI-SURFACE.md` implementation order. Cobra-style CLI:
- `bridge agent ask <slug> "<task>"` — calls bridge-server session API, waits for completion, returns final assistant text. **Highest leverage subcommand** — unblocks cross-harness dispatch.
- `bridge tools list / run` — proxy to tool-store.
- `bridge memory search / save / recent / show / forget` — proxy to memory-store.
- `bridge notes list / get / write / done` — proxy to noteboard.
- `bridge bus publish / tail` — proxy to bus.
- `bridge skills get <name>` — proxy to skill-store (lets header-only injection grab full bodies on demand).

**Why this is its own PR set:** different binary than `inber-cli`, different repo, different lifecycle. Can ship in slices — `bridge agent ask` alone unblocks cross-harness work; the rest follow as time permits.

**Effort:** 1-2 weeks if done as a single sprint; can stretch over more if sliced.

### P8 — `/agents` UI + agent-store CRUD API

**Repo:** `~/repos/agent-store` + `~/repos/bridge-ui` + `~/repos/dash` (+ llmux)

**Scope (decided 2026-05-10) — three core capabilities:**

1. **Agent identity editor** — markdown editor over `agent_nature` rows. Default: edits land in `kind=identity`. Structured-mode lets the user split into typed rows (`principle`, `value`, etc.).
2. **Visible-skills picker** — multi-select against skill-store catalog. Per-agent enrollment writes `agent_skills` rows. Renderer injects only names + descriptions; bodies stay grabbable on demand.
3. **Available-tools picker** — multi-select against tool-store. Per-agent enrollment writes `agent_harness_tools` rows.

Plus:
- Per-harness enrollment toggles (which harnesses this agent runs on) → `agent_harness` rows.
- Subagent tree view via `agent.parent_agent_id`. Add child agents from parent view.

**`/files` repositioned** as the debug / inspection surface — read-mostly view of materialized files, drift indicators, runner seeding state. Power-user edits still allowed; expected primary edit path is `/agents`.

**Implementation:**
- agent-store: REST endpoints for agent CRUD — audit current surface, fill gaps. Add `agent_skills` from P1.
- bridge-ui: new `BridgeAgents` component. Reuse existing patterns from `BridgeFiles`, `BridgeKanban`.
- dash: mount `/agents` route, proxy `/api/agent-store/*`.
- llmux: same wiring (per the `Dash hosts BridgeUI chat` memory — both consumers must be wired).

**Effort:** 1-2 weeks.

---

## Priority 5: Context migration (separate track)

### P9 — `llm-bridge/assembly` skeleton

**Repo:** `~/repos/llm-bridge` (decided 2026-05-10 — folds in here, not a standalone repo)

**Scope:**
- Define `Assembler` type, `TurnInput`, `HarnessHints`, `Budget`, `AssembleStats`.
- Stub `Assemble` method.

**Effort:** 1 day.

### P10..P15 — Leaf-first ports from inber

Each module is a separate small PR per `CONTEXT-MIGRATION.md`'s step 2:

- P10: `dedup`
- P11: `prune`
- P12: `refs` (extract.go)
- P13: `summarize`
- P14: `stage` + `repair`
- P15: `blueprint` + `assemble`

Each port keeps inber's copy as a thin wrapper that calls into the new library, so inber's tests stay green throughout.

**Effort:** ~2 weeks total, spread over time. Can interleave with other work.

---

## Priority 6: Codex (later)

### P16 — Codex native tool catalog audit

**Scope:** verify Codex's tool surface against a live install, fill out the table in `TOOL-ROUTING.md`. Identify Codex's native shell tool and confirm `bridge agent ask` invocation through it works as expected (the CLI-in-prompt pattern from `CLI-SURFACE.md`).

**Why deferred:** doesn't block CC vertical. Worth doing right rather than fast.

**Effort:** 1-2 days.

### P17 — Codex renderer + harness bridge

**Repo:** `~/repos/llm-bridge` + `~/repos/llm-bridge-codex`

**Scope:** parallel to P3+P4 but for Codex.

**Effort:** 5-7 days.

---

## What we're explicitly not doing (yet)

- No per-session workspace dirs. (Decision settled.)
- No CLAUDE.md write-back loops. agent-store writes; agents read. Edits go to agent-store via API or `/files`.
- No hot-reload of agent definitions into running sessions. Effects show next session.
- No cross-harness session migration mid-flight. Pick a harness at session create, that's it.

## Risk register

| Risk | Where | Mitigation |
|---|---|---|
| Schema rename touches every consumer | P1 | Run `grep -r orchestrator_id` across all repos before merging; bundle the rename + consumer updates as a single coordinated PR set. |
| `--agents` JSON argv size limit | P3/P4 | Filter by `agent_harness.subagent_allow`. Fall back to `~/.claude/agents/<slug>.md` if filter still produces a too-large bundle. |
| Existing context logic in `agents_context.go` is load-bearing for non-Claude harnesses | P6 | Don't delete until the assembly library (P15) is feature-complete. Run them side-by-side with a flag during transition. |
| inber's tests break during context-migration ports | P10..P15 | Keep thin wrappers in inber that delegate to the library. Don't remove inber's own copies until all consumers (bridge-server, inber itself) are on the library. |
| `/agents` UI scope creep | P8 | Time-box. v1 is read + edit, no fancy enrollment-conflict-resolution. Iterate. |
| Codex catalog reveals worse-than-expected tool surface | P16 | Codex is the second harness; if it's bad, document the gap and ship CC-only, push Codex parity later. |

## Suggested order of operations

If picking up this work cold tomorrow:

1. **Week 1** — P1 (schema rename), P2 (render skeleton). Both small, mostly mechanical, unblock everything.
2. **Week 2-3** — P3 (CC renderer), P4 (CC AgentReconciler), in parallel with P5 (inber-cli pass). End of this period: CC sessions can be spawned with the new layer working end-to-end.
3. **Week 3-4** — P6 (bridge-server wires AgentReconciler), P7 (BundleHash). Cuts over actual session creation to the new path.
4. **Anytime in parallel** — P9..P15 (context migration), P8 (/agents UI), P16 (Codex audit).
5. **Week 5+** — P17 (Codex), other harnesses (Gemini, Aider, Goose) as their scaffolds get fleshed out.

The cleanest "definition of done" milestone for the harness-layer arc: bridge-server can spawn a CC session for an agent-store-defined agent with subagents and tool/skill enrollment, with no per-session workspace directory, no CLAUDE.md write-back hacks, and the BundleHash logged for observability. That's P1 + P2 + P3 + P4 + P6 + P7 — roughly 3-4 weeks of focused work, less if the rename and skeleton land first and then the verticals run in parallel.

## Open decisions

All resolved 2026-05-10:

- ✅ Assembly library lives in `llm-bridge/assembly/` (folded into existing repo).
- ✅ Cross-harness subagent dispatch is allowed by default — renderer routes inline (same harness) vs. custom delegation tool (different harness) automatically per subagent's preferred harness.
- ✅ `/agents` UI scope = identity editor + visible-skills picker + available-tools picker (+ per-harness enrollment + subagent tree). `/files` becomes a debug/inspection surface.

No remaining open decisions blocking PRs.
