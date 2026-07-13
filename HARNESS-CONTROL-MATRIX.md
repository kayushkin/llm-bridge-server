# Harness Control Matrix

A coverage audit of llm-bridge against an external taxonomy of harness control
boundaries, taken from the Mojo AI Studio "Harness Skills" board
(<https://mojoaistudio.com/harnesses/>, captured 2026-07-13).

Companion doc: `inber/docs/harness-control-matrix.md` runs the same audit
against inber.

## Why this taxonomy is worth borrowing

The source framing: *prompt engineering changes the conversation, harness
engineering changes what the model is allowed and able to do.* A harness is the
operating layer around the model — files, tools, retrieval, memory,
orchestration, permissions, tests, traces, recovery — and every harness boundary
answers five questions:

1. **Intent** — what is this AI step supposed to accomplish?
2. **Inputs** — what context, files, memory, instructions are allowed in?
3. **Authority** — what model, tool, route, action permissions are allowed?
4. **Output** — what shape, evidence, trace, review state must come out?
5. **Recovery** — what happens when it fails, stalls, overspends, or needs review?

Their board enumerates **39 boundaries**, each with three orthogonal control
axes — **capability** (what it can do), **performance** (how fast), **cost** (how
much) — for 117 cells total. The boundaries split into two layers:

- **Layer 1, control plane** (coordinates sessions, work, authority): Orchestrator,
  Proof Authority, Tool Registry, Session Manager, Memory, Git Control, Release
  Readiness, Review Queue, Project Scope, Health Monitor, Interface, File
  Navigation, Guardrails, Network Protocol, Sub-agent Dispatch.
- **Layer 2, per-model-call** (governs one request end to end): Provider, Identity,
  Select, Route, Model Router, Fallback, Prompt, Context, Goal, Policy, Tools,
  Response, Evidence, Trace, Payload, Verify, Trust, Cost, Latency, Privacy,
  Safety, Compare, Knowledge Retrieval, Browser Capture.

**What is not worth borrowing:** the 117 downloadable `SKILL.md` files are
template-generated. Each is ~4.5 KB of an identical runbook skeleton; the only
per-file content is a one-line description and a 1–3 item lever list. Do not bulk
install them into `~/.claude/skills/`. The value is the boundary list and the
~250 distinct levers, which make a good audit checklist — which is all this doc
uses them for.

## The structural facts that drive the verdicts

**llm-bridge-server never makes a model API call.** It resolves a credential from
auth-store and hands it to a harness subprocess; `claude`/`codex` does its own
dispatch. The provider bridges (`llm-bridge-anthropic|openai|google`) are pure
format converters — `build.go`/`parse.go`/`stream.go`, no HTTP client — and have
no consumers outside `llm-bridge-openrouter`. Every boundary that lives at the
model-call site (Model Router, Select, Route, Latency, Fallback, Compare, Trust,
Payload, Provider transport) is therefore unowned. Recorded below as ABSENT
rather than N/A, because the session layer *could* own them and our own design
docs say it should.

**The flagship design docs are stubs.** `llm-bridge/render/claudecode.go` returns
`errors.New("claudecode renderer: not implemented (P3)")`. `bridge.AgentReconciler`
has zero implementations and zero callers. There is no `internal/toolrouting/`,
no `llm-bridge/assembly/`, no `cmd/bridge-cli/`, no `internal/orchestrator/`, and
no `BundleHash` anywhere. HARNESS-LAYER, TOOL-ROUTING, CACHE-RULES, CLI-SURFACE
and CONTEXT-MIGRATION are designed, not implemented: P2 (skeleton) landed, P3–P7
did not.

**The dominant failure mode is built-but-unwired, not absent.** memory-store (0
rows), the bundle resolver (dormant flag), `instance_tools` (inert),
`agent-store.model_fallbacks` (no consumer), `CreateSessionRequest.MaxBudget` (no
reader), `usage-store.Track()` (no callers). Several boundaries flip from ABSENT
to COVERED by connecting wires that already exist on both ends.

## Coverage

Tally: **3 COVERED · 18 PARTIAL · 18 ABSENT**.

| Boundary | Verdict | Owner | Missing levers |
|---|---|---|---|
| Browser Capture | ABSENT | — (playwright/chrome-devtools MCPs exist in tool-store) | all |
| Compare | ABSENT | — | all; bridge-ui side-by-side panes are independent sessions, not one prompt fanned out |
| Context | PARTIAL | CC's own compactor via `POST /sessions/{id}/compact`; `agents_context.go` | packing strategy, per-model window map, trim/summarize, chunk reuse; `llm-bridge/assembly/` does not exist |
| Cost | PARTIAL | `harness/derivation.go` (apiSpend*), `msg.APICallEvent`; model-store prices; `BridgeUsage.tsx` | budget pre-check, halt/alert thresholds, per-project budgets, demote/block. `MaxBudget` (`msg/server.go:264`) has no server-side reader; usage-store is not wired to the server at all |
| Evidence | ABSENT | — | all; `bridge verify` designed in CLI-SURFACE, unbuilt |
| Fallback | PARTIAL (thin) | `--fallback-model` pass-through; watchdog auto-resume | chain definition, retry policy, fast-fail, warm standby, tier cap. `model_fallbacks` has zero consumers |
| File Navigation | ABSENT | — | all; `git.go` does repo attribution, not code nav |
| Git Control | PARTIAL (read-only) | `internal/server/git.go`, `GitPanel.tsx`; `--worktree` pass-through | branch leases, diff-risk scoring, CI gates, main-write gate, conflict predict |
| Goal | PARTIAL | kanban-store + `kanban-scoper`/`-dispatcher` crons | goal schema on session, goal-context cache, off-goal pruning. TEAM-ORCH §7.1's `team_id`/`board_id`/`role` are not in `store.go` |
| **Guardrails** | **COVERED** | **permission-store** (rules, scope cascade, regex cache, `mvdan.cc/sh` Bash atom splitter, audit); `permission_prehook.go` (7 modes) | secret-pattern packs, credential-boundary policy, diff-only scan scope. **See the safety note below — the mechanism is strong, the live rule set is not.** |
| Health Monitor | PARTIAL | `server.go` `StartWatchdog` (60s), `reapIdleTick`, `autoResume` | custom health checks, alert routing, anomaly window, probe backoff (fixed 60s scan) |
| Identity | PARTIAL | model-store; `--model`/`--effort`; `APICallEvent.Model` records what ran | version allowlist, identity-record schema, fast/cheap pinning |
| **Interface** | PARTIAL | bridge-ui: 14 pages, 15 session states, hand-rolled SSE with `Last-Event-ID`; dash + llmux | no virtualization anywhere; no render throttle (1 render/SSE delta; `TurnsView` is O(n²)/render); ~10 polling intervals running *alongside* SSE; no panel plugin surface |
| Knowledge Retrieval | ABSENT | skill-store (registry, no routing); log-store FTS | all. memory-store's "semantic search" is a self-declared TF-IDF placeholder (`embedding.go:9`) |
| Latency | ABSENT | — | all. `DurationMS`/`DurationAPIMS` are recorded; nothing acts on them |
| Memory | ABSENT | memory-store, mounted at `server.go:220` | **everything.** Zero producers, zero consumers, DB has 0 rows, last touched 2026-05-10. Only real consumer is inber |
| Model Router | ABSENT | — | all; the server never dials a provider |
| Network Protocol | PARTIAL | `runner_ws.go` + `enroll.go` (WS + Bearer, single-use enrollment passphrase); sha256 binary integrity | capability advertisement, packet signing, handshake cache, discovery, degraded-mode fallback, remote-call budgeting |
| Orchestrator | PARTIAL | scheduler crons (`kanban-*`, `autoworker*`, jobs 13–19) | pluggable arbitration, priority weights, lane cap, preemption, attention budget, idle reclaim. `internal/orchestrator/` does not exist. **`max_concurrent_sessions` is stored and never enforced** (README admits it) |
| Payload | ABSENT | — | all. No hash/sign of the prompt. CACHE-RULES rule 7 mandates BundleHash; zero implementations |
| Policy | PARTIAL | `llm-bridge-jig` YAML profiles with `extends` (model/effort/budget/permissions/tools/skills/agents) — a real constraint-pack primitive | **no profiles exist on disk** (`~/.jig` empty); no authoring UI; no cheap-tier constraint |
| **Privacy** | **ABSENT** | — | **everything.** Zero redaction in the ecosystem. log-store persists full raw tool inputs/outputs forever, unredacted; permission-store's audit table does too (34 MB, 19,469 rows) |
| Project Scope | PARTIAL | repo-store `/detect` + bundle-store `/resolve` + `bundle_resolution.go` — implemented, tested, **on the spawn path** | **nothing sets `auto_bundle: true`**, so the whole chain is built, deployed, and 100% dormant. Resolved skills/model/effort are logged and discarded |
| Prompt | PARTIAL | `startOnInstance` (`hook_settings.go:22`) — a single chokepoint with 5 injections (hooks, agents-context, permission-mode, bundle, MCP) | template library, injection-order rules, precompile, cache-prefix reuse — all in the stubbed `render` package. Cache breakpoints are CC's, not ours |
| Proof Authority | ABSENT | — | all. TEAM-ORCH §20 designs the Stop pre-return gate and calls it *"the highest-leverage reliability step"* — unbuilt. `autoworker-reviewer` asks the same agent to grade itself |
| Provider | PARTIAL | **auth-store** (credentials, 1h leases + heartbeat, OAuth refresh, `key_access_log`, `keyAccess` middleware on every secret-returning call) | endpoints, regions, pooling, keepalive, batch-vs-realtime, compression — the vendor adapters have no HTTP client |
| Release Readiness | ABSENT | — (healthcheck + `repo-*-guard` jobs cover our own repos) | all. That's CI hygiene, not a gate on agent output |
| **Response** | **COVERED** | SSE fan-out with replay + subscriber eviction (`manager.go:560`); log-store (events + materialized history); `--json-schema` → `ResultEvent.StructuredOutput`; snapshot-store (content-addressed, 30d purge); hook-store post-hooks | delta-only storage, artifact compression |
| Review Queue | ABSENT | — | all. No severity rubric, no dedup, no reviewer set, no repair-verify gate |
| Route | ABSENT | `resolveInstance` picks any enabled instance — placement, not routing | route table, dispatch hooks, per-route token cap, dispatch concurrency |
| Safety | PARTIAL | permission-store deny rules; 7 modes; bridge-defined floor (`autoModeSafeTools`/`planModeTools`/`readOnlyTools`) | safety rule packs, risk-scoped guard depth. See safety note |
| Select | ABSENT | — | all. Model is whatever the caller put in `HarnessConfig` |
| **Session Manager** | **COVERED** | `store.go` + `harness/derivation.go` (13-state machine derived centrally, not re-implemented per consumer); create/send/interrupt/resume/stop/fork/compact/mode-switch; startup reconcile + `autoResume`; watchdog; idle reaper with process-group kill; PTY attach with ring-buffer replay | worktree-isolation rules (`--worktree` is pass-through; `forge` isn't wired to sessions); lifecycle hooks; resume-state prefetch |
| Sub-agent Dispatch | ABSENT | CC's native `Task` runs *inside* the harness — invisible to the server | **everything.** `msg.Event.HarnessParentID` is declared, but `llm-bridge-claudecode/process.go:195` explicitly sets `ParentToolUseID: nil` and the server has zero references to it. Subagents surface only post-hoc via `discover.go` scanning `~/.claude/projects/*/subagents/*.jsonl` |
| Tool Registry | PARTIAL | tool-store (kind mcp/cli/local, `input_schema`, `env_keys`, creds→auth-store, `/provision` → `--mcp-config`) | **zero safety surface** — grep for `permission\|sandbox\|dry.?run\|reversib\|ceiling` across tool-store returns 0 hits. Two dead wires: nothing writes `tool_store_tools`; `ProvisionRequest` has no `instance_id`, so the `instance_tools` opt-ins bridge-ui writes never reach a spawn |
| Tools | PARTIAL | `--allowed-tools`/`--disallowed-tools`; permission-store gates every call | tool-call ceilings (none anywhere), tool-result cache, parallel-safe-call policy |
| Trace | PARTIAL | log-store (every event, full payload); correlation ids (`BridgeSessionID`/`TurnID`/`MessageID`/`DerivedFrom`); OTel sidecar → `APICallEvent` | **no `trace_id`/`span_id`/`parent_span_id`** (TEAM-ORCH §14 confirms). **No async write** — `logStore.PushEvent` is synchronous in the event-read loop (`manager.go:536`), so a slow log-store back-pressures the harness pump. No sampling, no retention |
| Trust | ABSENT | — | all. auth-store has `error_count`, model-store has health — raw material, no state machine, nothing gates on it |
| Verify | ABSENT | — | all. `conformance/` verifies *harness protocol* (23 features), not response correctness |

## Safety note — the live rule set, not the mechanism

The permission-store *mechanism* is genuinely good. The rules currently loaded
into it are not. Three things compose:

1. `permission_prehook.go:178` — unattended sessions (`autonomous`, `herald`)
   auto-allow any `ask` outcome.
2. In the live DB (`~/.config/permission-store/permission-store.db`), both seed
   deny rules — `seed-v1-deny-bash-rm-rf-abs` (`^rm\s+-rf?\s+/`) and
   `seed-v1-deny-bash-curl-external` — are `enabled = 0`. Meanwhile a user rule
   allows `^curl\b` at priority 200 (vs. the deny's 50), another allows `^cat\b`,
   and `Write`/`Edit` are globally allowed. The only enabled deny in the entire
   table is one on `WebFetch`.
3. There is no tool-call ceiling and no budget halt anywhere in the stack.

Net: for an unattended autoworker session, permission-store blocks essentially
nothing — including `rm -rf /`. The prehook's own comment asserts the opposite
("deny rules already short-circuited above, so guardrails like `rm -rf /` /
curl-external still apply"); that assumption is false against the live rule set.
Those seed rules appear to have been switched off incidentally by "always allow"
banner clicks, which is exactly the failure mode a priority-ordered allow/deny
list invites.

## Ranked build list

1. **Cost — a budget pre-check and halt gate.** Every input already exists:
   model-store has per-million prices, usage-store has spend, `APISpendTotalEvent`
   has live per-session USD. Only the gate is missing. Today the sole cap is
   `--max-budget-usd` handed to the CC binary, and `CreateSessionRequest.MaxBudget`
   is a declared field no server code reads. An autoworker fleet with auto-allow
   can burn unbounded money with no server-side stop. Highest leverage per line
   of code in this audit.
2. **Verify / Proof Authority — the Stop pre-return gate.** Our own doc calls it
   the highest-leverage reliability step. Today `completed` means *the process
   exited*, nothing more, and the only backstop is `autoworker-reviewer` asking
   the same agent to grade its own work. Every closed kanban card is unverified.
3. **Sub-agent demux (`HarnessParentID`).** The field is already on the wire type;
   the claudecode bridge explicitly nils it. Wiring it makes CC's fan-out
   observable, gateable, and countable — and it is the prerequisite for lane caps,
   agent-count ceilings, *and* the trace tree.
4. **Turn on the bundle resolver.** repo-store + bundle-store + `injectBundleResolution`
   are built, tested, deployed, on the spawn path, and dormant because nothing
   sets `auto_bundle: true`. The cheapest "make an existing feature real" item here.
5. **Trace — `trace_id`/`span_id`, and make the log write async.** Two independent
   wins; TEAM-ORCH §14 already specifies the fields. Separately, `PushEvent` is
   synchronous inside the harness event pump, so a slow log-store throttles every
   live session.
6. **Tool ceilings + reversibility marks.** permission-store gates *whether* a call
   runs; nothing caps *how many*, nothing marks a tool irreversible, nothing
   dry-runs before spend. Combined with unattended auto-allow, a looping agent can
   hammer a destructive tool without bound.
7. **Memory — wire it or delete it.** Full schema (importance, decay, expiry,
   compaction, provenance) mounted on :8160 with zero producers, zero rows, and a
   placeholder embedder. It is a façade that currently *reads as coverage* in every
   architecture doc. Give it a producer at turn boundaries plus a real embedder, or
   remove it.
8. **Fix the two dead wires in tool provisioning.** Nothing writes `tool_store_tools`,
   so `/provision` never fires; and `ProvisionRequest` has no `instance_id`, so the
   per-instance opt-ins `BridgeTools.tsx` writes to `instance_tools` never reach a
   spawned session. The Tools page is currently UI-only state.
9. **Privacy + retention — they are one problem.** Zero redaction anywhere;
   log-store keeps full raw tool inputs/outputs forever with no prune, and
   permission-store's audit table stores raw Bash commands verbatim. One
   `cat ~/.aws/credentials` is now persisted in perpetuity and forwarded to logstack.
10. **Enforce `max_concurrent_sessions` + an attention ceiling.** The field is on
    every instance and the README calls it "informational — server-side enforcement
    is not yet wired up."
11. **Interface — virtualize and throttle.** No virtualization anywhere, one React
    render per SSE delta, `TurnsView` is O(n²) per render, ~10 polling intervals
    alongside SSE. The boundary most likely already hurting at high session counts.
12. **Land the `render` stack (P3–P7).** Prompt, Payload, and cache observability are
    all blocked on it. Until it lands, three boundaries stay structurally unownable.

## Honest summary

llm-bridge is excellent at what it set out to be — a *session and harness* layer.
Session Manager, Response, and the Guardrails mechanism are genuinely strong and
better than most systems have. But it is not a *request* layer: it never dials a
provider, so ~9 boundaries are ceded to the harness CLI by construction. That is a
legitimate architectural choice, and this matrix is not an argument to reverse it —
it is an argument to be explicit about which boundaries we have *chosen* not to
own, so that "we have no budget gate" stops reading as an oversight and starts
reading as a decision (or gets fixed).
