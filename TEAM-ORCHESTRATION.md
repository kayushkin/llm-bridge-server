# Team Orchestration

Dynamic, skill-formed agent **teams** that coordinate over a kanban board as a shared
blackboard. A planner decomposes a goal into role-tagged tasks; an orchestrator forms a
team of agents matched to the required capabilities; tasks are assigned and worked as
bridge sessions; agents coordinate cross-task by posting and claiming cards.

This is **not greenfield**. The `scheduler/cmd/kanban-*` loop (scoper → dispatcher →
curator → classifier) is already a single-board, generic-worker version of this. This doc
generalizes it into role-aware, per-team, blackboard-coordinated orchestration.

> Read after [`HARNESS-LAYER.md`](./HARNESS-LAYER.md) (session spawn contract),
> [`AGENT-MANAGEMENT.md`](./AGENT-MANAGEMENT.md) (agent shape), and
> [`CLI-SURFACE.md`](./CLI-SURFACE.md) (the `bridge` tool agents use to talk to the board).

---

## Reading order & section map

These sections were written incrementally; the numbers are **stable anchors** (cross-referenced
throughout), so rather than renumber — which would break ~100 internal references, the very §18
contract-first lesson — here is the intended *reading* path:

**Concept flow:** §15 front door & control loop → §3 role model → §13 recursion → §17 budget →
§18 write phase → §6 blackboard → §12 managed/unmanaged layers → §19 harness-vs-model → §14
observability → §20 verification & hooks.
**Then implementation:** §5 skill→tool · §7 bridge-server ties · §8 component map · §9 roadmap.
**Reference:** §16 consolidated roster — **the canonical role glossary; where any section's role
wording conflicts, §16 wins** · §10 open questions · §11 research basis.

§1–§2 are orientation (what exists, locked decisions); §4 is the slow-path component view — the
control flow proper is §15.

---

## 1. What already exists (the primitive)

| Piece | Lives in | Does today | Becomes |
|---|---|---|---|
| **scoper** | `scheduler/cmd/kanban-scoper/main.go` | Haiku decomposes a Goals card → 3–8 Backlog sub-cards, grouped by `group:<key>` | **Planner** — emits tasks tagged with a *required capability* + *role*, and provisions a team board |
| **dispatcher** | `scheduler/cmd/kanban-dispatcher/main.go` | Spawns/revives a bridge session per group, links `session`↔card, rolls children up to parent (`childrenSettlement`) | **Assigner** — spawns sessions of the *formed* archetype with task-scoped tools |
| **curator** | `scheduler/cmd/kanban-curator/main.go` | Moves In-Progress→Done/Failed from bridge session state | mostly unchanged |
| **classifier** | `scheduler/cmd/kanban-classifier/main.go` | Haiku observes sessions → cards | unchanged (observability) |
| **kanban-store** | `:8305` | Programmable boards (`POST /api/boards`), `card_links` to any entity (`entity_type=agent|session|skill|…`), cross-cutting `entity_tags` | the **team board + blackboard** |
| **skill/agent/tool stores** | `:8301` / lib / `:8302` | searchable skills, `POST /agents`, per-agent tool sets, `POST /provision`→MCP config | the **team-formation** inputs |

Board structure today (`Task completion loop`): `Goals | Backlog | In Progress | Done | Failed`.
Card = a noteboard item attached to a board via a `placement` row (kanban-store holds no card
body of its own — see kanban-store `internal/model/model.go:83` `Placement`, `:148` `CardLink`).

---

## 2. Locked decisions

- **Loop = hybrid.** Slow path (decompose, form team, roll up) stays cron-polled by evolving
  the `kanban-*` binaries. Fast path (agents posting/claiming blackboard cards) is
  **event-driven in bridge-server** so cross-agent hand-offs react in seconds, not on a 5-min tick.
- **Board = per team/goal.** One board is a team's shared workspace; tasks are cards, columns are
  stages, and the board carries the *semantic* coordination structure (who's doing what, blockers,
  hand-offs). A session carries `team_id` + `board_id` + `role` + its card link. The board does **not**
  replace session *lineage*: recursion and the in-flight trace still need `root`/`depth`/`parent` on
  sessions (§13, §14.2). The board holds meaning; the session tree holds structure.
- **Orchestration is harness-agnostic and native to bridge-server** (the fast path), reusing the
  existing session spawn machinery rather than any one harness's internal sub-agent feature.

("Orchestration" / "orchestrator" / "coordination engine" throughout this doc = *this* team-coordination
layer — **not** the deprecated `orchestrator` = per-runtime-registry term that `AGENT-MANAGEMENT.md`
renamed to "harness.")

## 3. The role model — context lifecycle, not job function

Empirical grounding (2025–26 multi-agent research, §11) points *away* from an org-chart of
functional agents (frontend/backend/security/testing) and *toward* roles defined by **how they
handle context**. The robust finding across every serious source:

> **Split reads, not writes.** Extra agents that *read / verify / think* help; multiple agents that
> *write in parallel and must stay mutually consistent* fail.

- Cognition: *"multi-agent systems work best when writes stay single-threaded and the additional
  agents contribute intelligence rather than actions"* — because *"actions carry implicit
  decisions… that might conflict."*
- Anthropic's research system beat single-agent by ~90% on parallel, read-heavy research (at ~15×
  tokens) but is *"less effective for tightly interdependent tasks such as coding."*
- MAST (200+ tasks, 7 frameworks): 79% of multi-agent failures are specification + coordination;
  36.9% are inter-agent misalignment, incl. *"context loss during handoffs."*

So `frontend` + `backend` as peer writers is the *documented failure case* (two writers sharing an
evolving contract); `security`/`testing` as verifiers is the *proven-valuable case*. We therefore
**do not** model the team as functional peers. Roles split along two orthogonal axes:

### 3.1 Write path = a context lifecycle (single-threaded)

One coherent write thread, broken into context stages — not parallel co-writers:

```
 SCOUT      ──► DISTILL ──► WRITE ──► [refresh @ phase boundary] ──► WRITE'
 (wide read,    (compress    (narrow      (drop stale history;        (reseeded with
  big context)   to working   context,     respawn FRESH session)      current architecture
                 set)         writes)                                   only)
```

- **Scout** reads widely to ground the task (discovery). Large context is fine — it writes nothing.
  (Planning/decomposition is a *separate* control-axis role — §15/§16 — not this stage.)
- **Distill** compresses what was learned into a *working set* for the writer. This is Cognition's
  *"compression LLM… compress a history of actions & conversation into key details, events,
  decisions"* and Anthropic's *"subagents return condensed findings."*
- **Write** runs with a deliberately *narrow* context — only the distilled working set + the live
  artifact. Single-threaded.
- **Refresh** — at a phase boundary, spawn a **fresh** session seeded only with the *current*
  architecture/state, discarding accumulated exploration history (failed attempts, superseded
  plans) that causes context rot.

Cheap on existing primitives: a refresh is **not a fork** (forks inherit history) — it's a *fresh*
session whose injected system prompt = the distilled current-state artifact, via the existing
`startOnInstance` / `injectAgentsContext` path (`hook_settings.go:22`, `agents_context.go:22`). The
dispatcher already chooses fork-vs-fresh; "refresh" is just *fresh, reseeded from the artifact*.

### 3.2 Verifier lenses = parallel readers off the artifact

Orthogonal to the write pipeline: `security`, `test`, `review`, `research/docs`. They run in
parallel, get **clean context** (just the artifact, not the writer's history — *"reviewers perform
better with completely clean context"*), and contribute **findings as cards, never writes**. This is
the generator-verifier loop the research endorses; the fresh-context reviewer advantage is
empirically confirmed (architect + security + QA "catches categorically more than a two-agent setup").

### 3.3 The load-bearing risk: distillation is lossy

The distiller decides what the writer gets to see — get it wrong and the writer is blind to
something load-bearing (the MAST "context loss" failure). **Principle: distill to a pointer-rich
index, not a prose summary.** The working set carries references (file:line, card ids, doc anchors)
so the writer can *re-fetch full detail on demand*, rather than a compressed paragraph that has
silently dropped it. The durable artifact (the team's architecture doc + board) survives every
refresh and is always re-fetchable; the *session* is ephemeral, the *artifact* is canonical.

### 3.4 Refresh trigger

Refresh on **semantic phase boundaries** (a subtask/card completes, or the plan materially
changes), **not** a raw change/token count — a counter can sever the session mid-reasoning. A
change-count is only the cruder fallback if phase boundaries are hard to detect.

### 3.5 Agent identity (resolves the earlier A-vs-B question)

This reframe settles it toward **seeded sessions, not minted agents**: lifecycle stages and verifier
lenses are *sessions seeded from artifacts*, not distinct agent-store identities. You need a *small
fixed set* of durable archetypes — the six LLM roles of §16 (planner, scout, distiller, writer,
task-evaluator, verifier-lens) — not an open-ended job taxonomy and not ephemeral per-task agents
(agent-store has no GC/TTL today). Skill→tool resolution (§5) becomes
*less* central on the write path — the writer's tool set is stable — and matters mainly for equipping
verifier lenses. agent-store stays the stable *identity* registry; ephemeral composition lives in the
**board + session** layer.

---

## 4. The slow-path pipeline (component data flow)

The *control flow* — how a goal enters and is triaged, planned, verified, and looped — is **§15**.
This section is the slow-path **component** view: the cron lifecycle the control loop drives.

```
  Goal card
     │  (slow path · cron)
     ▼
 ┌─────────┐   tasks tagged: capability:<x>, role:<r>, group:<k>
 │ PLANNER │ ─────────────────────────────────────────────────────┐
 └─────────┘   + creates team board (per goal)                     │
     │                                                             ▼
     ▼                                                      ┌──────────────┐
 ┌──────────────┐  capability → skills → tools → roster     │  TEAM BOARD  │
 │ TEAM-FORMER  │ ─────────────────────────────────────────▶│ (blackboard) │
 └──────────────┘  records roster on board                  └──────────────┘
     │                                                          ▲   │
     ▼                                                          │   │ (fast path · events)
 ┌────────────┐  spawn session(archetype, task-tools)          │   ▼
 │ ASSIGNER   │ ── link session↔card, move → In Progress ──────┘  ┌───────────────────┐
 └────────────┘                                                   │  COORDINATION     │
     │                                                            │  ENGINE           │
     ▼                                                            │ (in bridge-server)│
 ┌──────────┐  session state → Done/Failed, parent rollup        │ agent posts/claims│
 │ CURATOR  │ ◀──────────────────────────────────────────────────│ card → inject into│
 └──────────┘                                                     │ teammate session  │
                                                                  └───────────────────┘
```

### Stage detail

1. **Planner** (upgrade scoper). Produces a **coarse** task list (not exhaustive — decomposition is
   lazy, §15.2), each sub-card tagged `capability:<x>` + `role:<r>` (not just `group:`). Creates the
   team board (`POST /api/boards`) and links it to the goal card. The sketch / triage / Plan-Critic
   that precede this are §15.

2. **Team-former** (new — slow path). For the distinct capabilities in the plan, resolve a roster:
   `capability → skills → tools → archetype`. See §5 for the skill→tool resolution. Records the
   roster on the board (one `role:<r>` card per seat, or board-level `entity_tags`) so the assigner
   and the coordination engine can see who's on the team.

3. **Assigner** (upgrade dispatcher). For each role-tagged task, spawn a bridge session of the
   chosen archetype, link `session`↔card, move to In Progress. Reuses the dispatcher's existing
   revive-window and session-id minting. Each task's write work runs the **context-lifecycle inner
   loop** (§3.1: scout→distill→write→refresh); **verifier lenses** (§3.2) attach to the board and
   run in parallel off the artifact.

4. **Coordination engine** (new — fast path, in bridge-server). §6. Reacts to blackboard card
   events: a posted request/blocker/handoff is routed to an idle teammate session by injecting a
   message via `harness.Manager`; a claim links the card to the claiming session.

5. **Curator / rollup** (reuse). Unchanged closure + `childrenSettlement` parent rollup. On full
   settlement, archive the team board.

---

## 5. Prerequisite — skill → tool resolution (on the critical path)

"Form a team based on available skills" is blocked by a real gap: **skills and tools have no
mapping**. `agent-store` has a dormant `agent_skills` table with zero CRUD (`schema.sql:98`), and
skill-store search is substring-only (`server.go:143`). Two ways to close it:

- **(a) Static `skill_tools` map.** Add a table/registry mapping each skill → tool names. Precise,
  cacheable, but must be authored and maintained as skills are ingested.
- **(b) LLM resolver at formation time (recommended for v1).** The team-former (Haiku) reads the
  candidate `SKILL.md` bodies (skill-store already stores `SkillMD`) plus the tool-store catalog
  (`GET /tools`) and *picks* the tool set. No mapping table to maintain; promote hot
  skill→tool results into a cache table later if cost warrants.

Either way the output feeds `tool-store POST /provision` → MCP config, merged into the session's
`HarnessConfig` at spawn (`hook_settings.go:22`, same path as `injectAgentsContext`).

---

## 6. The blackboard protocol (fast path)

Agents coordinate by **writing cards**, not by direct messaging. Three coordination card types
(distinguished by an `entity_tag`):

| Type | Meaning | Routed to |
|---|---|---|
| `request` | "I need capability X / a teammate to do Y" | an idle teammate session with matching `role`, else surfaced to the team-former to add a seat |
| `blocker` | "I'm blocked on task Z / another agent's output" | the owner of Z's card (via its `session` link) |
| `handoff` | "Done with my part, next is yours" | the next role in the workflow |

**Claim semantics.** A card in an `Unclaimed`/`Requests` column is claimed by an agent linking
itself: `POST /api/cards/{id}/links {entity_type:"session", entity_ref:"<sid>"}` + an
`entity_tag` `claimed`. First writer wins; the coordination engine enforces single-claim.

**Agent-facing surface.** Agents touch the board through the `bridge` CLI (see `CLI-SURFACE.md`),
routed into every harness per `TOOL-ROUTING.md` (`native > MCP > CLI`):

```
bridge kanban post   --board <id> --type request|blocker|handoff --title … [--role …]
bridge kanban claim  --card <id>
bridge kanban list   --board <id> [--role <r>] [--unclaimed]
bridge kanban done   --card <id> [--summary …]
```

This is the harness-agnostic glue: one tool works for CC, codex, aider — no per-harness sub-agent
feature required.

---

## 7. Where it ties into bridge-server

### 7.1 Sessions schema — `internal/store/store.go:104`
Add three columns (distinct from the existing `spawner_id` / `parent_id` / `purpose`):

- `team_id` — the goal/team identity (= the board's owning goal card id). Cheap "list my team".
- `board_id` — the team board this session coordinates on.
- `role` — archetype/role this session is filling (`builder`, `reviewer`, …).

**Consolidate these with the lineage fields from §14.2** (`root_session_id`, `depth`, a
`manager_session_id` index) — one migration, not two. The board + `card_links` hold the *semantic*
structure; these columns hold *lineage* for recursion (§13) and the in-flight trace (§14).
**Canonical session-id names + the full model: §21** (`bridge_session_id` / `manager_session_id` /
`forked_from_session_id` / `refreshed_from_session_id` / `root_session_id`, plus what is *not* a session id).

### 7.2 Spawn injection — `internal/server/hook_settings.go:22` (`startOnInstance`)
The existing single chokepoint for every spawn. Extend it to:
- stamp `team_id` / `board_id` / `role` onto the session,
- merge the task's provisioned tools/skills (from §5) into `HarnessConfig` — same mechanism as
  `injectAgentsContext` (`agents_context.go:22`) and hook/MCP injection.

Note: per `CONTEXT-MIGRATION.md`, the prompt-shaping in `agents_context.go` is migrating into the
`llm-bridge/assembly` library. The `startOnInstance` chokepoint **stays**, so this tie-in point is
stable — only the `injectAgentsContext` internals move behind a library call.

### 7.3 Coordination engine — new `internal/orchestrator/` (fast path)
A subscriber that consumes **kanban card events** and reacts. Wiring:
- kanban-store emits a card-changed event (NATS on the existing bus, or a webhook) on
  create/move/claim. *(kanban-store has no event emit today — small add.)*
- the engine resolves the target teammate session and **injects a message** via
  `harness.Manager` (claudecode already supports message injection; harnesses without injection
  degrade to next-poll and the engine `log`s the limitation rather than dropping the hand-off).

### 7.4 Routes — `internal/server/server.go:107`, beside `/sessions/{id}/fork`
- `GET /teams/{goal_id}` — sessions + board rollup for a team.
- `GET /sessions/{id}/team` — the session's teammates.
- (board CRUD stays in kanban-store; bridge-server only reads/links.)

### 7.5 Agent CLI — `CLI-SURFACE.md`
Add the `bridge kanban …` subcommands (§6) and allowlist them per the CLI-surface permission
patterns.

---

## 8. Component map — by trigger plane

Every moving part, grouped by *how* it fires (roles: §16; hooks: §20.4; verification: §20.1).

| Plane | Trigger | Component | Home |
|---|---|---|---|
| Slow | cron `*/5` | Planner (was scoper) — sketch + coarse plan | `scheduler/cmd/kanban-scoper` |
| Slow | cron `*/5` | Assigner (was dispatcher) | `scheduler/cmd/kanban-dispatcher` |
| Slow | cron `*/5` | Curator + rollup (reuse) | `scheduler/cmd/kanban-curator` |
| Slow | per plan | Team-former (**new**) — roster resolution | folded into Planner or own binary (§10) |
| Per-node | control | Plan-Critic (mostly deterministic) | bridge-server + verification runner |
| Per-node | the write loop | Scout → Distiller → Writer → Task-Evaluator (§3.1) | bridge sessions (L1) |
| Parallel | artifact exists / Review column | Verifier lenses (§3.2) | bridge sessions (L1) |
| Verification | per stage (§20.2) | structural / acceptance / e2e runners | tool-store + `verify` / conformance infra |
| Fast | card event | Coordination engine (**new**) | `llm-bridge-server/internal/orchestrator` |
| In-session | hooks (§20.4) | PreToolUse gate+budget · PostToolUse · SessionStart · **Stop (pre-return)** · PreCompact · SubagentStop | bridge-server hook endpoints |
| Pulled | agent CLI | `bridge kanban` · `bridge verify` · provisioned MCP | the `bridge` binary / tool-store |

---

## 9. Sequenced roadmap

Low-risk-first. The write-path mechanism and ground-truth come early because they gate quality;
team-formation / skill-routing come later (they mostly equip verifier lenses).

> **Depends on** the harness-layer critical path (`IMPLEMENTATION-ROADMAP.md` P1–P6, esp. the `bridge`
> CLI at P5b and session-spawn tool/skill injection). Steps 1–2 below ride the *current* cron loop and
> need none of it; steps 5–9 do.
>
> **Rollout discipline — every increment ships *off by default behind a toggle*** (flag / env), so
> rebuilding or redeploying a live binary changes nothing until the toggle is flipped. Toggle-off must
> be byte-identical to the prior behavior; each feature keeps a kill switch. (Step 1's scoper
> `capability:`/`role:` tagging is gated by `--role-tags` / `KANBAN_SCOPER_ROLE_TAGS`, default off.)

1. **Board-per-team + coarse planner tags (§4, §15.2).** Upgrade scoper to create a team board and tag
   tasks `capability:`/`role:`. No new session behavior — testable on the existing loop.
2. **Context-lifecycle write path (§3.1).** Distiller + phase-boundary refresh (fresh reseeded session
   via `startOnInstance`, not a fork). Testable with a single builder. *Core mechanism milestone.*
3. **Verification runners + acceptance-criteria-on-cards + the Stop pre-return gate (§20).** Ground-truth
   early — it gates every "done" and kills false-completion. *Highest-leverage reliability step.*
4. **Verifier lenses (§3.2).** Clean-context security / test / review / research reading the board.
   *First generator-verifier loop.*
5. **Sessions schema — team/board/role + root/depth + `parent_id` index (§7.1 + §14.2, one migration)
   + `/teams` & tree routes.**
6. **Recursion + write-invariant + per-node pooled budget (§13, §17).** decompose-vs-do, single-writer
   enforcement, the token meter.
7. **Skill→tool resolution (§5b).** LLM resolver — scoped mainly to equipping verifier lenses.
8. **Blackboard protocol (§6).** `bridge kanban` CLI + claim semantics (still cron-latency).
9. **Coordination engine + event hooks (§7.3, §20.4).** kanban event emit → `internal/orchestrator` →
   `Manager` injection; PostToolUse / SessionStart / PreCompact / SubagentStop. *Turns 5-min hand-offs into seconds.*
10. **In-flight observability — trace plumbing + bridge-ui tree (§14).**

Steps 1–4 ride the working cron loop and are low-risk; steps 9–10 are the genuinely new infrastructure
(event emit, in-server subscriber, trace/span fields).

## 10. Open questions

- **Distillation format** (§3.3) — pointer-rich index (recommended) vs prose summary. How much full
  detail to inline before falling back to a re-fetchable reference.
- **Refresh trigger** (§3.4) — phase-boundary (recommended) vs change/token count; how to detect a
  "plan materially changed" signal cheaply.
- **skill→tool: resolver vs map** (§5) — recommendation (b) resolver for v1; revisit if cost/latency bites.
- **Team-former placement** — its own `scheduler/cmd` binary, or folded into the planner? (Leaning
  separate, to keep decompose and roster-resolution independently testable.)
- **Event transport** for §7.3 — reuse the NATS bus (via `llm-bridge-adapter`) vs a direct
  kanban→bridge webhook.
- **Entry cost at volume** (§15.1) — the single good-model sketch assumes human-paced intake. A
  high-volume *automated* goal source (classifier / autoworker / scheduler) could justify a cheap
  pre-filter — though those sources already gate upstream, so likely still not worth the misroute risk.
- **Where the durable artifact lives** (§18) — the architecture-doc/contract needs a physical home: a
  noteboard note, a repo file, or a board-pinned card. Unspecified; pick one before §18 is buildable.
- **Human-in-the-loop / escalation** — when does the orchestration pause for a human (risky action,
  blocked plan, root-budget exhaustion)? Wire to the existing autoworker approval gate + herald `ask` channel.
- **Cross-team isolation** — two goals touching the same repo/files. The §3 write-invariant covers one
  subtree, not two teams; per-team/per-node forge worktrees are the likely answer.
- **Idempotency on cron re-entry** — the 5-min loop must not double-spawn; restate the existing
  `entity_tag` dedupe (`scoped`, sig-tags) for the new role-aware flow.
- **Goal-level failure / abort** — partial-results rollup to the user when a whole goal fails or the
  root budget exhausts, distinct from per-task re-plan.

## 11. Research basis

- [Why Do Multi-Agent LLM Systems Fail? (MAST, arXiv 2503.13657)](https://arxiv.org/abs/2503.13657)
  — 14 failure modes; 79% spec+coordination, 36.9% inter-agent misalignment incl. handoff context loss.
- [Cognition — Don't Build Multi-Agents](https://cognition.ai/blog/dont-build-multi-agents) — single-threaded
  writes; a compression LLM for context.
- [Cognition — Multi-Agents: What's Actually Working](https://cognition.ai/blog/multi-agents-working)
  — "writes single-threaded, extra agents contribute intelligence not actions"; share-fork vs clean-context.
- [Anthropic — How we built our multi-agent research system](https://www.anthropic.com/engineering/multi-agent-research-system)
  — orchestrator-worker, ~90% gain on parallel research at ~15× tokens; weak on interdependent coding.
- [MetaGPT (arXiv 2308.00352)](https://arxiv.org/html/2308.00352v6) — structured-artifact handoff,
  "communicative dehallucination."
- [Anthropic code-review agent teams](https://devops.com/anthropic-code-review-dispatches-agent-teams-to-catch-the-bugs-that-skim-reads-miss/)
  — architect+security+QA catches categorically more; fresh-context reviewers.
- [ADaPT: As-Needed Decomposition and Planning (arXiv 2311.05772)](https://arxiv.org/abs/2311.05772)
  — decompose *only on executor failure*, recursively, with AND/OR sub-task composition; +28/27/33% over baselines.
- [Kambhampati — LLMs Can't Plan, but Can Help Planning in LLM-Modulo Frameworks (arXiv 2402.01817)](https://arxiv.org/pdf/2402.01817)
  — LLMs can't self-verify plans; a *separate* external verifier in a generate-test-critique loop does (Blocks World → 82%).
- [LangChain — Plan-and-Execute Agents](https://www.langchain.com/blog/planning-agents)
  — replan when an observation contradicts a planning assumption / tools fail persistently; current state as new start.
- [Adaptive LLM routing survey (arXiv 2502.00409)](https://arxiv.org/html/2502.00409v3)
  — route by estimated complexity; reserve the expensive path for the fraction that needs it.
- [BAMAS: Budget-Aware Multi-Agent Systems (arXiv 2511.21572)](https://arxiv.org/pdf/2511.21572)
  — demand-driven pooled budget beats static partition; static *fails at depth 5+* (dynamic +20–35%);
  hard caps + soft escalation + reflow of unused budget; over-funding already-solved tasks is pure waste.
  *(Exact percentages are benchmark-specific; the qualitative findings are what's load-bearing here.)*
- [Hierarchical budget controls for multi-tenant LLM gateways](https://dev.to/pranay_batta/building-hierarchical-budget-controls-for-multi-tenant-llm-gateways-ceo)
  — layered budgets (team ▷ repo ▷ dev), a spend debits *all* matching rules, total never exceeds parent.
- [Lost in the Middle (Liu et al., arXiv 2307.03172)](https://arxiv.org/abs/2307.03172) — >30% accuracy
  drop when key info sits mid-context; U-shaped attention; all frontier models still degrade with length.
- [Anthropic — Effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
  — compaction (summarize-and-restart, recall-then-precision), structured note-taking (Notes.md as external memory), clean-context sub-agents.
- [Skeleton-of-Thought (outline-first generation)](https://learnprompting.org/docs/advanced/decomposition/skeleton_of_thoughts)
  — outline then expand; coherent for long-form, but parallel expansion *breaks on interdependent code* — fill sequentially against a frozen contract.
- [Anthropic — Scaling Managed Agents: decoupling the brain from the hands](https://www.anthropic.com/engineering/managed-agents)
  — model decides & *pulls* context from a durable log; harness owns durability/recovery (`wake`), stateless tool exec, credential isolation.
- [Anthropic — Effective harnesses for long-running agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
  — harness owns env scaffolding, progress/feature tracking, verification tooling, session-init protocol; failure modes: false "done", undetected broken feature.

---

## 12. Two control layers — bridge-managed vs harness-coupled

**Every subagent becomes a first-class bridge session** — including a harness's own (CC `Task`). Each
gets its own `bridge_session_id` and a `manager_session_id` link to its parent (§21). What differs is
**who owns its process lifecycle** — i.e. whether bridge-server can independently control it. The
boundary is **control, not existence.**

- **L1 · bridge-managed** — bridge-server spawned the process; it can pause / kill / refresh / re-route
  it directly.
- **L2 · harness-coupled** — the harness spawned it *inside the parent's process* (CC `Task`). The
  **harness adapter promotes it to a bridge session** (mints `bridge_session_id`, sets
  `manager_session_id` = parent), so it's visible, nested, and attributable like any session — **but
  its lifecycle is the parent's:** you can't pause/kill it alone, only by acting on the parent.

A session attribute records the difference: **`lifecycle_owner` ∈ {`bridge`, `harness`}** (derivable
from "has its own OS process," but explicit is honest).

> **vs `AGENT-MANAGEMENT.md` / `HARNESS-LAYER.md`.** Those treat CC subagents as first-class *managed
> config* — defined in agent-store (`parent_agent_id`, `subagent_allow`), rendered into `--agents`. That
> *definition* layer stays true. This section is the *runtime instance*: a defined subagent, once `Task`
> spawns it, is now a real (harness-coupled) bridge session, not an opaque span.

| | L1 · bridge-managed | L2 · harness-coupled |
|---|---|---|
| Own `bridge_session_id` | yes | **yes (adapter-minted)** |
| `manager_session_id` → parent | yes | **yes** |
| Linked to a kanban card | yes | yes |
| `lifecycle_owner` | `bridge` | `harness` |
| Independently pause/kill | yes | **no — only via the parent process** |
| Observability | full event stream | **full, once the adapter demuxes** (§21.4); CC-only until other harnesses expose subagents |
| Recursion bounded by | server config | the harness (its own depth, still opaque) |

**Implications:**

- **The tree is uniform.** §14 observability is just the `manager_session_id` session tree — no special
  "span" reconstruction. The old "L2 = spans, not sessions" model is **superseded**: L2 is a session,
  marked `lifecycle_owner=harness`.
- **Promotion lives in the harness adapter** (§19 — the harness owns its own translation). For CC, the
  adapter demuxes the subagent stream and mints a `bridge_session_id` per subagent; demux key in §21.4.
- **Still prefer L1 for anything you must *steer*.** A `harness`-lifecycle session is observable but not
  independently killable — for a verifier lens / refresh / sub-team you need to control, spawn it L1.
  (`HARNESS-LAYER.md`'s "prefer the native `Task` dispatcher" stays right for an agent's internal leaf fan-out.)
- **The runaway blind spot closes.** Harness-coupled depth was invisible; once promoted it's visible
  (you still can't *kill* a deep L2 chain except via its root parent — control ≠ visibility).
- **The line is living** (§19.3): as models gain native sub-agents, more work is harness-coupled —
  promotion keeps it *visible* even when it isn't independently *controllable*.

## 13. Recursion — one node shape, with a write invariant

The current pipeline (goal → tasks → sessions) is a hardcoded ~2-level org. Replace it with a single
**recursive orchestration node**; the top-level goal and a leaf task are the *same shape* at different
depths. This is inber's spawn model (`MaxSpawnDepth`, `MaxChildrenPerAgent`, `parent.Children`,
async `SpawnResult` — `inber/server/spawn.go`) generalized to the board+session layer.

A **node** = `{ owning session(s), a board (its workspace), parent node, root, depth, budget, children[] }`.
At each node the planner/distiller makes one decision: **decompose vs do.**
- *Do* — the task is small enough; the writer just does it (leaf). Recursion bottoms out.
- *Decompose* — spin up a child node (its own scout→distill→write + verifier lenses on a sub-board).

This removes the fixed level-count: complex tasks grow their own sub-teams; simple ones don't.

### The invariant that keeps recursion safe

> **Recurse freely on the read/verify axis; the write axis is a single active chain.**

Scouts, researchers, and verifier lenses (§3.2) may fan out and nest arbitrarily — they produce no
conflicting writes, and this is exactly where multi-agent *wins* (Anthropic's parallel research).
But the **write path (§3.1) must stay single-threaded down any root-to-leaf chain**: a node may
*delegate* its write downward (one child writes while the parent waits) but must **not** have two
write-active descendants in the same subtree at once. That parallel-writer subtree is the documented
failure mode (§3, MAST/Cognition). The monitor treats "≥2 write-active sessions in one subtree" as an
**invariant violation to surface**, not a silent state (§14).

### Budgets are pooled, not pre-partitioned (full model in §17)

Tokens are a **shared pool drawn down on demand**, not divided among children up front — static
partitioning fails at depth, and the planner can't predict per-child cost anyway. Depth and breadth
are separate *hard* caps; tokens are the soft pooled resource with per-node caps + escalation. The
decompose-vs-do decision above factors **remaining pool**: a node that can't fund a subtree biases to
*do*, or fails up. This is what stops recursion from becoming a fork-bomb.

## 14. In-flight observability — the trace none of this has yet

Today there is **no cross-session trace/span plumbing** (confirmed): `msg.Event` is flat per
session+turn (`llm-bridge/msg/event.go` — has `BridgeSessionID`/`TurnID`/`MessageID`, **no**
`trace_id`/`span_id`/`parent_span_id`); the sessions table has only `parent_id` (harness UUID,
forward-only) + opaque `spawner_id`, **no** root/depth/children index; log-store is per-session;
logstack receives result summaries only (`log-store/internal/logstack/forward.go:76`). A live tree
view is essentially net-new — but it hangs off three sources that already exist.

### 14.1 The span model — node ⊃ session ⊃ harness-task

Three nesting levels map onto a standard span tree:

```
  trace = the root orchestration node (the goal)
   └─ span: orchestration node (depth N)        [L1, from sessions tree]
       └─ span: session (a writer / verifier)   [L1, from sessions table + event stream]
           └─ span: harness-internal task        [L2, CC task_started→task_progress only]
```

- **Node & session spans** come from the **managed tree** (§14.2 fields).
- **Harness-task spans** come from the **per-session event stream** (the `task_*` system events),
  nested under their session span — CC only; other harnesses contribute a bare session span with no
  children, correctly reflecting that L2 is opaque there.
- **Semantic state** (what each node is *doing*, blockers, hand-offs) comes from the **kanban board**
  — cards/columns/links are already the blackboard (§6).

### 14.2 Plumbing to build

1. **Event fields** (`llm-bridge/msg/event.go`): add `TraceID` (= root node id), `SpanID`,
   `ParentSpanID`. Stamped at `startOnInstance` so every managed spawn is causally linked to its
   parent across the session boundary — the link that's missing today.
2. **Sessions tree fields** (`internal/store/store.go` sessions): add `root_session_id`, `depth`, and
   a **partial index on `manager_session_id`** (where non-empty) for cheap children lookups.
   (Joins the `team_id`/`board_id`/`role` from §7.1; canonical session-id names in **§21**.)
3. **Tree queries + endpoints**: `GetSessionChildren` / `GetSessionTree` / `GetAncestry`; expose
   `GET /sessions/{id}/tree`, `/children`, `/ancestry`. None exist today.
4. **A trace-scoped live stream**: an SSE that fans in events across a whole node subtree (the
   per-session SSE at `sessions.go:427` exists; the aggregate does not), so the monitor streams the
   *tree*, not one session.
5. **bridge-ui**: a collapsible node/session tree with live state, plus a board pane — the existing
   `BridgeKanban` *is* the semantic view; the new piece is the lineage tree beside it.

### 14.3 What the in-flight view must show

- **The live tree**: each node/session with role, depth, state (running/idle/blocked), token burn,
  elapsed. L2 tasks shown nested + badged **"observed, not managed."**
- **The blackboard pane**: open requests / blockers / hand-offs (cards), so coordination is legible.
- **Drill-down**: click a node → its live SSE event stream.

### 14.4 Alerts (the point of monitoring in flight)

- **Stuck node** — idle/awaiting beyond threshold (the autoworker-hang class of bug).
- **Write-invariant violation** — ≥2 write-active sessions in one subtree (§13). Surface loudly.
- **Runaway depth/breadth or budget exhaustion** — incl. the L2 blind spot: a managed session
  burning tokens with no managed children almost always means a deep L2 fan-out you can't see —
  flag on token-rate, since the subagents themselves are invisible.
- **Blocker pile-up** — N cards parked in a `blocker`/`request` column = coordination deadlock.

### 14.5 Honest limits

L2 fidelity is per-harness and unchangeable from our side: CC gives task narration, codex/inber/goose
give nothing. You cannot control an L2 subagent without killing its L1 parent. The monitor must show
this boundary rather than imply a uniform tree — an opaque L1 session is a *correct* rendering of a
harness that doesn't surface its internals, not a gap to paper over.

---

## 15. The front door — triage, planning, and the control loop

This is the top of the system: how a goal becomes (or doesn't become) a team. The research here is
specific, and some of it pushes back on the obvious design.

### 15.1 Triage *after* a cheap sketch — not a blind pre-classifier

An earlier draft put a complexity *classifier* before planning. That's the weak version: you can't
reliably estimate decomposability before you've looked — it's exactly the unverifiable LLM judgment
LLM-Modulo warns against. And the systems we cite actually triage *during* planning: Anthropic's lead
agent assesses complexity and subagent count *as part of* its planning/thinking; ADaPT discovers
complexity by *attempting* and decomposing on failure. The plan (or a sketch of it) is the best
complexity signal — so plan-ish first, then decide.

There is **no separate cheap pre-filter.** A classifier gating everything is the LLM-Modulo
anti-pattern (an unverifiable cheap judgment whose misroute sends real work to a single unplanned
session); on turn one — low context — a light sketch by the *good* model is already nearly free, fires
only once per intake, and saves nothing meaningful over the classifier it would replace. It would also
be **redundant** with triage, which already routes "small → single session, no board." So the entry is
a single good-model pass, and triviality is just one of its outcomes:

1. **Scoping sketch** — one light pass by the **good model** (the Planner in sketch mode). It either
   *handles a trivial request directly* (answer / do it, single session, done) or emits a *rough plan
   shape*: task count, coupling, areas touched, candidate roles. Cheap on turn one because context is low.
2. **Triage on the sketch** — for real work, decide on *evidence*, not a vibe (see §15.1a).
3. **Full plan + Plan-Critic + board + team** — the expensive, hard-to-reverse commit, only past triage.

Triage sits *between a cheap plan and the expensive commit*. The board, team-former, and critic — the
costly, hard-to-reverse parts — are what's gated; the looking is always done, by the good model. (The
one case that could reopen a cheap pre-filter is high-volume *automated* intake — see §10.)

### 15.1a What triage decides: decomposition granularity, not solo-vs-team

Triage isn't binary. Read off the sketch's size/coupling, it picks the **unit of decomposition** —
which *is* choosing §13's recursion granularity at plan time:

| Sketch shape | Decompose at… | Resulting shape |
|---|---|---|
| small, low-coupling | **task scope** | one node, leaf tasks, a single writer (often no board) |
| medium | **task scope + verifier lenses** | one node + a board, generator-verifier loop |
| large / multi-area | **agent scope** | top node fans into child *nodes* (sub-teams), each its own board (§13) |

So "how large the plan gets" directly sets whether you plan *tasks* or *agent workstreams*: a large
plan decomposes into sub-*nodes* (recursion), a small one into *tasks* (leaves). This granularity dial
is set once from the sketch and re-set whenever a re-plan changes the shape.

### 15.2 Plan coarse, decompose as-needed (don't over-split upfront)

The intuition "during planning, split into all the tasks" is the **wrong default.** Two findings
converge against exhaustive upfront decomposition:

- MAST: finer decomposition → *more* inter-agent coordination failures.
- ADaPT: decompose **only when the executor fails** the task, recursively, composing sub-tasks with
  an **AND/OR** operator (AND = all needed, OR = alternatives).

So the planner produces a **coarse** task list + a dependency graph, and §13's recursion does the rest
*lazily*: a task is attempted; only on failure does it spawn a child node that decomposes further.
This keeps the tree minimal — exactly the property that reduces coordination failure. The
"decompose vs do" decision of §13 is therefore **execution-failure-driven**, not pre-computed.

### 15.3 Verify the plan with a *separate* critic

LLMs can't self-verify plans (LLM-Modulo). The planner therefore hands off to a **Plan-Critic** —
a distinct role — in a generate-test-critique loop before any execution starts. Bias the critic toward
**deterministic/tool checks first**, LLM judgment only for what they can't cover:

- *deterministic*: do referenced files/paths exist? dependency graph acyclic? does each task have
  acquirable context? do roles map to available archetypes/skills?
- *LLM*: is the decomposition coherent, are acceptance criteria meaningful, are there obvious gaps?

Reject → back to the planner with the critique. This is the single highest-leverage place to spend a
verification pass, because a bad plan multiplies downstream.

### 15.4 The control loop (where re-plan / re-discover / compact live)

```
 INTAKE(goal)
   └─ SCOPING SKETCH (good model, light)
        ├─ trivial ─► handle in this session, done
        └─ real work ─► TRIAGE (shape → granularity: task · task+lenses · agent/sub-nodes §13)
              └─► PLAN (coarse tasks + AND/OR dep graph + roster) ─► PLAN-CRITIC ─reject─► PLAN
                    └─accept─► CREATE BOARD + tasks + form team
                          └─► per ready task:  SCOUT ─► DISTILL ─► WRITE ─► TASK-EVAL
                                                                              │
                                       success ─► mark done, roll up ◄────────┤
                                       failure ─► classify:                   │
                                            ├─ too complex ───────────► DECOMPOSE (recurse child node §13)
                                            ├─ grounding stale ───────► RE-DISCOVER (re-scout subtree) + reset
                                            ├─ assumption contradicted► RE-PLAN (current state = new start)
                                            └─ transient ─────────────► retry
   COMPACT/REFRESH (§3.4) fires at: phase boundary · after a RE-PLAN · after SCOUT gathers a lot · context bloat
```

The trigger taxonomy, made explicit:

| Event | Response |
|---|---|
| Executor can't do a task | **decompose** it (recurse a child node) — ADaPT |
| Observation contradicts a planning assumption | **re-plan** the remaining tasks from current state |
| Writer/scout finds the codebase ≠ what the plan assumed | **re-discover** (re-run Scout for that subtree) + reset |
| Phase boundary / plan changed / context bloat | **compact/refresh** — fresh reseeded session (§3.4) |
| Transient tool/error | retry, no structural change |

### 15.5 Two evaluators, two scopes

"Where do evaluator agents go" has two answers, and they're different roles:

- **Plan-Critic** (§15.3) — verifies the *plan*, once per plan/replan, before execution. External-checks-first.
- **Task-Evaluator** — verifies a *task result*, after each write. It owns the ADaPT success
  heuristic (did this succeed?) and therefore *drives* the control loop above: success → roll up;
  failure → decompose/re-plan/re-discover. Kept separate from the writer (a writer can't judge its
  own output any more than a planner can verify its own plan).

These are distinct from the §3.2 **verifier lenses** (security/test/review), which assess *quality
dimensions* of a completed artifact in parallel — not *did this task complete*. Three evaluation
roles, three jobs: plan correctness, task completion, artifact quality.

## 16. Consolidated agent roster

Every distinct role across the design, and — importantly — **which are LLM agents vs deterministic
code.** Design principle (LLM-Modulo + cost): *don't make something an LLM agent if a deterministic
check or a cheap classifier suffices.* Fewer LLM judgment calls = lower cost + smaller MAST failure surface.

| Role | Kind | Axis | Context profile | Writes code? | Spawned when |
|---|---|---|---|---|---|
| **Planner** (sketch → full) | LLM agent | control | sketch: goal + light context · full: goal + scout digest | no (writes plan artifact) | sketch every intake; full past triage |
| **Triage** | deterministic (reads sketch) | control | the sketch | no | after the sketch |
| **Plan-Critic** | mostly deterministic + thin LLM | verify · plan | clean (plan + spec) | no | after each plan/replan |
| **Scout/Discovery** | LLM agent | write-path · read | wide, ReAct-style | no | node start; on grounding drift |
| **Distiller** | LLM agent (or model-assisted) | write-path | wide-in → narrow-out | no | before each write / refresh |
| **Writer** | LLM agent | write-path | narrow (working set + artifact) | **yes — single-threaded** | per leaf task |
| **Task-Evaluator** | LLM agent + tool checks | verify · task | clean (task spec + result) | no | after each write |
| **Verifier lenses** (security/test/review/docs) | LLM agents + tools | verify · artifact | clean | no (findings → cards) | parallel, once a writable artifact exists |
| **Team-former** | deterministic + LLM resolver (§5) | infra | — | no | plan needs roles not in pool |
| **Assigner/Dispatcher** | deterministic code | infra | — | no | per ready task |
| **Coordination engine** | deterministic code | infra (fast path) | — | no | on blackboard card events |

The actual *LLM agents* (the expensive, fallible ones) are just six: **Planner, Scout, Distiller,
Writer, Task-Evaluator, Verifier-lens** — and the Planner doubles as the cheap scoping sketch (§15.1),
so triage costs no extra agent. Everything else is routing, resolution, or plumbing that should stay
deterministic. That's the cast — kept deliberately small.

## 17. Budget — a pooled draw, not a pre-partition

The recursion of §13 needs a budget model, and the obvious one (a parent *splits* its allowance among
children) is the wrong one.

### 17.1 Why not pre-partition

- **Static split fails at depth.** BAMAS: uniform allocation is adequate for 2–3 levels but *fails
  catastrophically* at 5+, where dynamic reallocation recovers 20–35%. A recursive design can't adopt
  the one strategy that breaks under recursion.
- **The planner can't weight it.** Planner-weighted allocation is an a-priori cost *prediction*, and
  we established (§15.1) that complexity isn't reliably knowable before doing the work. Weighted
  splits are guesses — they strand budget on cheap children and starve expensive ones.
- **Even split strands budget.** Most children finish under allocation; the remainder sits idle
  instead of funding the sibling that needed more.

### 17.2 The model: shared pool, demand-driven, capped (cgroups-style)

1. **Root pool.** Each goal gets one token pool at the root — from the human's directive (the Workflow
   `+N` pattern) or a default tier. Hard ceiling: total descendant spend ≤ pool; a child can never
   out-spend the root (hierarchical, like the multi-tenant-gateway pattern).
2. **Draw, don't reserve.** Children draw from the pool *as actually spent*; unused budget from a
   finished subtree **reflows** to pending siblings. No stranding — the efficiency win over splitting.
3. **Per-node cap.** A hard cap (a node may draw ≤ X% of the pool) so one greedy subtree can't starve
   siblings — this fixes pure-pooling's ordering/starvation weakness. Optional *floors* for
   fairness-critical siblings; caps + reflow is the 80/20.
4. **Structural limits are separate and hard.** Depth and breadth are cheap hard caps (decrement per
   level / per node); tokens are the soft pooled resource. Two budget kinds — *structural* (hard,
   counted) vs *consumable* (pooled, metered).

### 17.3 Enforcement — deterministic, BAMAS-style three tiers

The **orchestrator/coordination engine** owns the meter, reading live token usage off the event
stream (bridge already aggregates per-session usage — `llm-bridge-claudecode`). A node *asks* "may I
spawn / continue?"; the orchestrator answers from the meter. Budget stays judgment-free infra (§16).

- **Pre-flight check** — before decomposing, verify the remaining pool can fund the planned children;
  if not, the node biases to *do* over *decompose* (§13) or fails up. Never spawn a subtree it can't fund.
- **Soft escalation** — warn as a node nears its cap; on exhaustion with the task unfinished, the
  **Task-Evaluator (§15.5)** decides: grant more from the pool on re-plan, or roll up *partial*
  results. Graceful — never an abrupt mid-write cut.
- **Don't over-fund** — extra budget on an already-passing task is waste (BAMAS: over-funding solved
  tasks burns budget for no accuracy gain). The evaluator grants more only on *evidence of
  incompleteness*; each node returns best-effort, escalation is earned not default.

### 17.4 Two budget layers (tie to existing infra)

- **Per-goal pool** — §17.2, the orchestration budget.
- **Global provider ceiling** — usage-store (`:8185`) + the 7-day provider window the **autoworker**
  already respects. Per-goal pools must fit under it: the orchestrator defers/throttles *new* teams
  when the window nears exhaustion (reuse the autoworker's usage signal + ramp curve). A spend debits
  *both* its goal pool and the global window (layered budgets) — so a fleet of teams can't quietly
  drain the account.

### 17.5 Distillation is a budget mechanism, not just a quality one

Cost compounds because every child result feeds back into the parent's context on rollup, and input
tokens are charged on every turn — a deep tree pays to *re-ingest* descendants repeatedly. So §3.3's
distillation (the parent ingests a child's pointer-rich *digest*, not its full trace) is a primary
budget lever, not only a context-quality one — as is §3's clean/narrow context discipline
(isolated subagents cost materially less than context-accumulating ones). Model-tiering compounds it:
frontier for orchestration, cheaper models for *execution under a verified plan* (checkable output) —
never for an unverifiable gate (§15.1).

One pool per goal, drawn on demand, capped per node, escalated by evidence, nested under the global
provider ceiling — deterministically enforced.

## 18. Building a multi-section system — section progression & the four context moves

This is the **write phase** in detail: how one writer (or a chain of them) builds a system with
several significant sections. It sits inside the §3.1 write path (scout→distill→write→refresh),
*after* planning/triage (§15) and *before* the evaluators (§15.5) settle each section.

### 18.1 Section by section, not all at once

For anything with multiple significant sections, **build section by section.** The research is
one-sided: every frontier model still loses accuracy as context grows, and "lost in the middle" means
a model under-attends to the *middle* of a large context — i.e., the early sections of a monolithic
build. A single ballooning session therefore writes *worse* as it goes, and silently neglects what it
wrote first. Section-by-section keeps each write in a small, high-attention context, gives the
Task-Evaluator a natural cadence, and lets context be reset between sections (impossible in one
monolith). One-shot is right *only* for a small, tightly-coupled single artifact where sectioning
would fracture one coherent thing (and §3's write-invariant already forbids splitting a tight coupling).

### 18.2 Contract first, then fill against a frozen contract

Order matters. Do a **skeleton/contract pass first** — define the interfaces, types, schemas, and
module boundaries spanning all sections — and **freeze it** as the durable artifact. Then fill each
section *against the frozen contract.* This is skeleton-of-thought adapted for code: SoT's weakness is
that parallel sections can't see each other's internals and break on interdependence — freezing the
contract *removes the need to see internals* (each section codes to the contract, not to its
siblings). It also gates parallelism cleanly: **only once the contract is frozen and sections share no
mutable state may sections fill in parallel; otherwise sequential** (per §3's single-writer invariant).
Contract-first is also the cheapest place to catch a structural error — the Plan-Critic (§15.3) reviews
the contract before any fill, because a contract revision mid-fill ripples through every section.

### 18.3 The four context moves are not interchangeable

Your four mechanisms map onto **two axes** — *in-section vs cross-section*, and *same-role vs
different-role*:

| Move | Scope | Mechanism | Use when |
|---|---|---|---|
| **Prune** | in-section, same session | drop specific dead weight (stale tool outputs, a failed attempt, a now-irrelevant file) — surgical, no summary | junk accumulates mid-section; cheapest, do first |
| **Compact** | in-section, same session | summarize-and-continue (recall-first, then trim — Anthropic) | a *single* section's work itself overflows and prune isn't enough |
| **Reset / refresh** | cross-section, **same role** | fresh session reseeded from the artifact; transcript discarded (§3.4) | moving to the next section as the same kind of worker — sheds context rot entirely; the **default inter-section move** |
| **Handoff** | cross-section, **different role** | fresh session of a *different* archetype + a curated package | section is a different concern, or write→verify, or write→integrate |

Discriminator: **stay in-session while the work continues (prune→compact); go fresh at a section
boundary (reset/handoff).** Same role → reset; different role → handoff. Reset and handoff are the same
mechanism (a fresh, artifact-seeded session) pointed at the same vs a different archetype.

Cache note: the in-session moves (prune, compact) rewrite the prompt prefix and so **bust the prompt
cache** (per `CACHE-RULES.md` that's an accepted cost). A fresh *refresh* re-establishes a cacheable
prefix from the artifact, which is why repeated mid-session compaction is often dearer than one reset.

### 18.4 What crosses a section boundary (the payload)

A reset/handoff carries the **durable artifact**, not the transcript:

- the **frozen contract** / global architecture (§18.2),
- the **interfaces** of completed sections — *not their internals* (re-fetchable from code/board on
  demand, the §3.3 pointer principle),
- the **next section's spec** + any open cross-section decisions.

The board + architecture doc *is* the persistent memory across every reset — this is Anthropic's
**structured note-taking** (a `Notes.md`/board written outside the context window, re-injected when
needed). Prior sections' internal exploration is deliberately dropped; only the contract and the
interfaces survive.

**Pull, don't push.** The harness owns the durable log/board but does *not* decide the keep-set by
truncating. The model **pulls** what it needs from the store and **signals** the reset/compaction with
its own digest; the harness executes and persists but **never blind-truncates**, because only the model
knows what's load-bearing (§19.2). The harness exposes a retrieval/transform interface over the log
(Anthropic's `getEvents` pattern), not a fixed pre-chewed window.

### 18.5 Ordering & invalidation

Build in **topological order** over the dependency graph from planning: contract-defining sections
first, dependents after, so a later section can't invalidate an earlier frozen contract. If a section
*does* reveal the contract was wrong → **re-plan** (§15.4) — expensive, because it ripples — which is
exactly why contract-first + the Plan-Critic exist: to make that rare. If a section finds the codebase
≠ what the contract assumed → **re-discover** that slice (re-scout), don't guess.

### 18.6 How this ties in

- A "significant section" is usually its own **node** (§13): contract-first is the parent node's work;
  each section-fill is a child node's scout→distill→write→eval; the parent sequences them via §18.3.
- The cross-section moves are also **budget moves** (§17.5): reset/compaction stop you re-paying input
  cost on stale context every turn — context hygiene and cost control are the same lever here.
- All of it is the §3.1 write path, specialized: §3.1 says *refresh at phase boundaries*; §18 says a
  section boundary **is** the canonical phase boundary, and names which of the four moves to use at it.

## 19. Capability layer — harness (hands) vs model (brain)

A capstone lens, and the generalization of §16: every part of this system belongs to either the
**harness** (deterministic substrate — "hands") or the **model** (fallible reasoning — "brain"). The
design is already mostly organized this way; this section makes the division explicit and audits it.

### 19.1 The division of labor

| Harness owns (hands) | Model owns (brain) |
|---|---|
| **Mechanism** — spawn/kill/reset sessions, inject & transform context, load tools | **Judgment** — is the plan sound? did the task succeed? |
| **Persistence & recovery** — append-only log, board, artifacts; survive crashes (`wake`) | **Generation** — plans, code, distillations |
| **Enforcement** — budget caps, write-invariant, permission/usage ceilings | **Selection** — what context is relevant, what to keep/drop |
| **Ground-truth** — run tests/compilers/smoke checks, file-exists, dep-cycle | **Discovery & decomposition** — scout, read, split as-needed |
| **Observability** — the cross-session trace/span tree (§14) | **Depth** — understand one task deeply |
| **Isolation** — credentials never reach the model's sandbox | |
| **Concurrency execution** — run N sessions in parallel | |

Underneath: **the harness has breadth and durability but no semantic depth — it sees every session,
all state, and survives crashes, but it sees *events*, not *meaning*. The model has depth but no
breadth — it understands its task but sees only its own window and dies when it ends.** Orchestration =
harness supplies breadth + persistence + ground-truth; model supplies depth + judgment + generation;
they meet at a structured interface (board, distilled artifacts, typed events) — never by one doing the
other's job.

### 19.2 Four harness roles the obvious framing under-weights

Context fragmentation/sectioning, concurrency, tool/skill loading, overflow handling — all correctly
harness. Four more, each a major harness-only capability:

1. **Ground-truth / verification — the highest-leverage harness role, not plumbing.** The harness can
   *run* tests/compilers/smoke-checks and *know* what the model only guesses. Anthropic's two
   long-agent failure modes — declaring work "done" while incomplete, and not noticing a feature is
   broken end-to-end — are both fixed by harness verification, not better prompts. This is the
   LLM-Modulo external verifier (§15.3) as infrastructure; it's what makes an unreliable model reliable.
2. **Durability & recovery.** Append-only log, git rollback to known-good, progress/feature tracking, a
   session-init protocol (read progress → pick next → smoke-test). The model is stateless and dies; the
   harness is what makes long work survivable.
3. **Pull, don't push, context** (§18.4). Harness owns the durable log + a retrieval/transform
   interface; the **model pulls** what it needs and **signals** the reset. The harness must **never
   blind-truncate** — only the model knows what's load-bearing. This is the control channel §18/§3.4 needed.
4. **Concurrency is harness-*enabled* but model-structure-*safe*.** The harness runs N parallel
   sessions; *when* parallel writes are safe is semantic (the dependency graph + §13 write-invariant).
   The harness enables concurrency and enforces the limit; only model-derived structure says when it's allowed.

### 19.3 The boundary moves — design for it

Today the harness wins at context surgery and sub-spawning because models can't yet reliably manage
their own context or spawn parallel work. That's changing fast (native context editing/memory, native
sub-agents/Task tools, bigger windows, parallel tool calls), so work migrates **L1→L2** (§12). The
stable rule: **keep at the harness what needs enforcement, observability, persistence, or ground-truth;
cede to the model what needs its private knowledge of its own context's relevance.** Don't build
elaborate harness machinery to do the model's selection job (it'll be obsoleted and it's worse at it) —
and never trust the model to do the harness's enforcement or ground-truth job. §12's managed/unmanaged
line is exactly this seam; treat it as living, and degrade gracefully.

### 19.4 Audit — where the plan holds, where to sharpen

| Element | Split | Verdict |
|---|---|---|
| §16 roster (agents vs infra) | *is* the division of labor | ✓ the seed of this whole lens |
| §5 skill/tool loading | model resolves, harness provisions | ✓ clean |
| §3 context fragmentation | model distills, harness injects clean context | ✓ clean |
| §17 budget | harness meters/enforces, model escalates | ✓ clean |
| §14 observability | pure harness | ✓ clean |
| §15 triage | model sketches, harness routes | ✓ clean |
| §15.3 / §15.5 evaluators | harness ground-truth + model judgment | ✓ Anthropic's failure modes confirm keeping these |
| §18 / §3.4 context moves | mechanisms harness, decisions model | ⚠ sharpened to *pull-not-push* (19.2 #3, §18.4) |
| §12 managed/unmanaged | the boundary itself | ⚠ sharpened to *living seam* (19.3, §12) |
| ground-truth tooling | harness runs tests/smoke/compile | ✓ elevated to a first-class responsibility in **§20** (runners + acceptance-criteria-on-cards + the Stop gate) |

Net: the plan was already ~80% organized along this axis — the lens *validates* more than it breaks.
The corrections were pull-not-push context, the living L1/L2 boundary, and naming ground-truth tooling
as a first-class harness responsibility — the last now built out in **§20**.

## 20. Verification & the hook/trigger map

Where ground-truth (§19.2 #1) actually runs, what tooling the model gets, and every trigger in the
system. First, a distinction the question blurs — there are **two trigger planes**:

- **In-session hooks** (CC PreToolUse / Stop / …) — fire *inside one L1 session*, per turn / tool.
- **Cross-session triggers** (kanban cron + the coordination engine + scheduler) — fire *across*
  sessions, on board / lifecycle events.

The "auto-trigger before returning to the user" is the first kind (a Stop hook); the kanban
auto-triggers are the second.

### 20.1 The verifier is a deterministic runner, not an agent

Ground-truth tooling is a harness **runner** that executes a declared check-set and returns structured
pass/fail + output — no model judgment in the loop. Three tiers by cost/scope:

| Tier | Checks | Cost | Cadence |
|---|---|---|---|
| **Structural** | compile / typecheck / lint / dep-cycle / file-&-symbol-exists / schema-valid | sub-second | continuous (PostToolUse) |
| **Acceptance** | the task's unit/acceptance tests — *authored as failing tests up front* (Anthropic's "feature list"; the definition-of-done externalized) | seconds | per task, before "done" |
| **End-to-end** | run `init.sh` + browser/HTTP smoke — "does it actually work end-to-end" | tens of sec | per section / pre-return |

Acceptance checks live **on the kanban card** (a structured acceptance field the Planner writes). They
start failing and must pass — which kills Anthropic's false-"done" failure mode: completion is a
*tested* fact, not the model's opinion. Wiring: register the runners in **tool-store**; reuse the
existing **`verify` skill / conformance** infra for the e2e harness; results feed the **Task-Evaluator
(§15.5)**, which adds LLM judgment *only* on top of the ground-truth.

### 20.2 When verification fires — by orchestration stage

| Stage | Verifier tier | Fires | On fail |
|---|---|---|---|
| **Plan** (§15.3 Plan-Critic) | structural, on the *plan* (files exist, graph acyclic, roles resolvable) | before any execution | reject → re-plan |
| **Per task, before "done"** (§15.5 Task-Evaluator) | acceptance + structural | after each write | decompose / re-plan / re-discover (§15.4) |
| **Section boundary** (§18) | e2e / smoke for the section | before reset/handoff, and again at next session's init | don't carry a broken section forward |
| **Pre-return-to-user** | acceptance + e2e + verifier-lenses (§3.2) | when the top node would return | loop back — never surface a false "done" |
| **Continuous** | structural | PostToolUse after edits | fast tripwire, surface inline |

The model can also *pull* verification on demand (`bridge verify`, §20.3) — but the stages above
**auto-run** it via hooks, so verification never depends on the model remembering to.

### 20.3 Tooling exposed to the model

Two channels:

- **Stable cross-cutting tools** via the `bridge` CLI (CLI-SURFACE.md; designed, not yet built) — one
  allowlist entry `Bash(bridge:*)`. Add to its surface `bridge kanban post|claim|list` (§6 blackboard)
  and `bridge verify task|smoke` (§20.1), alongside the planned agent-ask / tools / memory / notes / bus / skills.
- **Task-specific tools** via tool-store `POST /provision` → per-session MCP config (§5), injected at
  spawn (`tool_provision.go`, `hook_settings.go`).

### 20.4 The hook map — today vs what this needs

**Today exactly one hook is wired:** **PreToolUse**, the permission gate at
`POST /permission/cc-prehook/{bridge_id}` (`server.go:159`), injected per session with three scopes
(global ▷ instance ▷ session) at `hook_settings.go`. PostToolUse / SessionStart / Stop / PreCompact /
SubagentStop are CC-supported but **not wired**. There is **no Stop (pre-return) hook today.**

| Hook | Lifecycle point | Role in this orchestration | Status |
|---|---|---|---|
| **PreToolUse** | before a tool call | permission gate *(exists)* **+ budget pre-flight (§17.3) + write-invariant enforce (§13)** — "may I spawn/continue?" | extend existing |
| **PostToolUse** | after a tool call | structural ground-truth tripwire (compile/lint) + feed usage to the budget meter | **new** |
| **SessionStart** | session boot | session-init protocol (read board/progress → pick next → smoke inherited state) — how a reset/handoff bootstraps (§18) | **new** |
| **Stop** | turn done, **before returning to user** | the **pre-return verification gate** (§20.2): acceptance + e2e + lenses; fail → loop back, don't return false "done" | **new — highest value** |
| **PreCompact** | before compaction | model emits its digest first → compaction is model-directed, not blind (pull-not-push, §19.2 #3) | **new** |
| **SubagentStop** | an L1 child node ends | trigger rollup + parent's distillation ingest (§3.3, §13) | **new** |

The single most valuable addition is the **Stop hook as the pre-return gate** — the trigger you
intuited, absent today, and exactly where false-"done" is caught at the user boundary.

### 20.5 Kanban / cross-session triggers

Today **all cron**: scoper / dispatcher / curator every 5 min, classifier every 15 min
(`scheduler/cmd/*`); no events. This orchestration makes it **hybrid** (the §2 decision):

- **cron** keeps the slow lifecycle — scope, dispatch, curate, rollup.
- the **coordination engine** (§7.3) adds the first *event-driven* kanban trigger: card create / move /
  claim → immediate routing (the blackboard fast path).
- a card entering a **Review** column auto-spawns verifier-lens sessions — the cross-session
  counterpart of the in-session Stop gate.

So: verification runs at five stages, surfaced both as a pulled `bridge verify` tool and as auto-firing
hooks (chiefly the new **Stop** pre-return gate); tooling reaches the model via `bridge` + provisioned
MCP; triggers span two planes — in-session CC hooks (one wired today, five to add) and cross-session
kanban triggers (cron today, hybrid cron + event under this plan).

## 21. Session ids — identity, lineage, and the node tree

The word "parent" hid ≥4 different relationships; conflating them caused the fork-vs-team mixup, and the
ground-truth audit found that one column (`parent_id`) wasn't even a session id. This is the canonical
session-id model. **§7.1 and §14.2 defer to it.**

**Naming convention:** every FK column **names its target entity type as a suffix** — `_session_id`
(a `bridge_session_id`), `_card_id`, `_board_id`, `_harness_id`. Labels and scalars take **no** `_id`.
So a reader never guesses whether a column points at a session — and a `_session_id` column **always
holds a `bridge_session_id`, never a harness UUID.**

### 21.1 The session ids

| Field | Was | Holds | Created when | Why it exists | Status |
|---|---|---|---|---|---|
| **`bridge_session_id`** | `bridge_id` | our own id | create / fork / discover (NOT resume) | the stable anchor; routing key on every event; the target of every `_session_id` FK | rename |
| **`harness_session_id`** | (same) | the harness's UUID | first harness event; **rotates** on resume & fork | drives `--resume`/`--fork`; harness-boundary only — never a lineage FK | keep |
| **`manager_session_id`** | `spawner_id` | the **managing/parent session** (`bridge_session_id`) | at spawn, set to the parent node; **null** = top-level | the **management tree** — who owns me; nesting + `root`/`depth` derive from it | rename + re-type |
| **`forked_from_session_id`** | `parent_id` | the fork source's **`bridge_session_id`** | on fork | conversation-branch lineage (with history) | rename + **re-key** |
| **`refreshed_from_session_id`** | — | the refresh predecessor's `bridge_session_id` | on a §3.1 refresh (fresh reseed, no history) | reseed continuity across context resets | net-new |
| **`root_session_id`** | — | top of my tree | at spawn (denorm from the `manager` chain) | cheap "whole team" queries without walking up | net-new |

Two invariants:
- **A lineage field exists only where bridge-server mints a *new* `bridge_session_id`** — fork and
  refresh. **Resume reuses the same `bridge_session_id`** (audit-confirmed: `handleResumeSession` /
  `autoResume` UPDATE, no INSERT), so there is **no `resumed_from`**; the `harness_session_id` rotation
  is a harness-adapter detail, not lineage.
- **`forked_from_session_id` is a `bridge_session_id`**, not the harness UUID. The UUID needed for
  `--fork` is read **live from the parent row at fork time**, so there is **no `fork_source_harness_id`** column.

### 21.2 Not session ids (don't lump them in)

- **`origin`** — entry surface / creating service (`frontend-dash`, `autoworker`, `scheduler`). A
  label, completely distinct from `manager_session_id` (a session). **Both kept** — not interchangeable.
- **`type`** (interactive / autonomous / system) · **`purpose`** (chat / subagent / conformance …) ·
  **`role`** (builder / reviewer / …) · **`lifecycle_owner`** (`bridge` / `harness` — who can
  start/stop this session's process, §12) — classification labels.
- **`depth`** — scalar. **`agent_id`** / **`instance_id`** — FKs to *other* entities (an agent identity,
  a harness instance), reused across many sessions. **`team_id`** / **`board_id`** — scope FKs.
- Turn/message scope (never session ids): `turn_id`, `message_id`, `harness_message_id`, `client_request_id`.

### 21.3 The three node kinds (the folder structure)

The sidebar tree differentiates three kinds **structurally — no `is_subagent` flag needed**:

| Kind | What it is | Identified by | In the sidebar |
|---|---|---|---|
| **Top-level** (user goal / chat) | a session row | `manager_session_id IS NULL` | a folder root |
| **Bridge-server subagent (L1)** — task-assigned | a session row | `manager_session_id IS NOT NULL` | nested under its manager; labelled by `role` + `type`/`purpose`; real → selectable, killable |
| **Internal subagent (L2)** — spawned inside the harness | a session, **adapter-promoted** | own `bridge_session_id` + `manager_session_id`, `lifecycle_owner=harness` | nested under its manager like any subagent, badged "internal"; visible/attributable but **not independently killable** (§12) |

Nesting keys on `manager_session_id` — uniform for L1 and L2. `role`/`type`/`purpose`/`lifecycle_owner`
give labels/icons. Forks are a *separate* lineage (`forked_from_session_id`), shown as a branch, not a
managed child.

### 21.4 Promoting harness subagents to sessions (the adapter's job)

The harness adapter **mints a `bridge_session_id` per subagent and links it** (`manager_session_id` =
parent) *before events reach bridge-server* — so a harness subagent arrives as a real session (§12),
not a span. Verified gap in `llm-bridge-claudecode` today (your suspicion was right): a live CC Task
subagent's events are stamped with the **parent's** `bridge_session_id` (one CC process = one bridge
session), and the field that distinguishes them is dropped — `ccStreamEvent` (`translate.go:15`) parses
only `type/subtype/session_id/message/result`, so CC's **`parent_tool_use_id`** is never read. So today
subagents collapse onto the parent.

The fix, in the adapter:
1. **Pick the demux key** — *verify against a live capture* whether CC stamps subagent stream lines with
   the **subagent's own `session_id`** (demux by that) or the **parent's `session_id` + `parent_tool_use_id`**
   (demux by that). The current code ignores per-line `session_id` variation *and* lacks `parent_tool_use_id`.
2. **Mint + map** — on first sight of a new subagent (new demux key), mint a `bridge_session_id`, record
   `key → bridge_session_id`, set `manager_session_id` = the parent's `bridge_session_id`,
   `lifecycle_owner = harness`.
3. **Re-stamp** every subsequent subagent event with the subagent's `bridge_session_id`.
4. Add `parent_tool_use_id` (and/or `harness_task_id`) to `ccStreamEvent` + canonical `msg.Event` so the
   key survives translation. bridge-server **auto-creates** the row on first-seen (live equivalent of the
   discovery upsert).

The parent's `task_started`/`task_progress` stay on the *parent* (it narrates "I spawned a task"); the
subagent's own work goes to the subagent's session. Net: the §14 tree is uniform sessions — no special
span path.

### 21.5 Migration

- `bridge_id` → `bridge_session_id`; `parent_id` → `forked_from_session_id` (re-keyed to a
  `bridge_session_id`, drop the stored harness UUID); `spawner_id` → `manager_session_id` (re-typed to a
  real session FK; **null** for top-level). `origin` unchanged.
- Net-new: `manager_session_id` semantics, `refreshed_from_session_id`, `root_session_id`, `depth`,
  `team_id`/`board_id`, `role`.
- Touches bridge-server (store, `sessions.go` fork plumbing, resume path), `ManagedSession` (Go +
  `llm-bridge/ts`), the UI. Rides **§9 step 5**; keep `bridge_id`/`parent_id`/`spawner_id` as deprecated
  aliases for one release.
- **Open behavioral choice:** does `manager_session_id` **inherit on fork** (a fork of a managed session
  stays under the same manager) or default **null** (a user fork is an unmanaged branch)? Recommend
  **null on fork** — a fork is a user/content branch, not a management relationship; an
  orchestration-driven sub-spawn sets `manager_session_id` explicitly.
