# Context Management Migration — inber → bridge-server

## Premise

bridge-server today has a context-management adapter (`internal/server/agents_context.go`, `bypass_inject.go`, scattered helpers) that is "messy and less complete." Inber has a real, production-tested context management stack (`engine/turn_*.go`, `conversation/`, `memory/`) that handles reference-based prompt assembly, smart truncation, tool pruning, summarization, repair, dedup. It's the better implementation.

The migration: **lift inber's context layer into a reusable form, retire bridge-server's adapter, and have inber's own runtime depend on the same library.** Single canonical implementation.

This is independent of the subscription-mode work. Even sessions that route through inber-the-runtime benefit, because both paths use the same context library.

## What we're moving

Mapping inber's directories to the concerns they cover:

| Directory | What it does | Migration target |
|---|---|---|
| `inber/engine/turn_prepare.go` | Pre-turn context assembly | Library: `llm-bridge/assembly/assemble` |
| `inber/engine/turn_prompt.go` | Prompt blueprint construction | Library: `llm-bridge/assembly/blueprint` |
| `inber/engine/turn_context.go` | Per-turn context state | Library: `llm-bridge/assembly/turn` |
| `inber/engine/prompt_blueprint.go` | System prompt + sections | Library: `llm-bridge/assembly/blueprint` |
| `inber/conversation/manage.go` | Conversation lifecycle | Library: `llm-bridge/assembly/conversation` |
| `inber/conversation/manage_tool_pruning.go` | Tool result pruning | Library: `llm-bridge/assembly/prune` |
| `inber/conversation/dedup_files.go` | File-content dedup | Library: `llm-bridge/assembly/dedup` |
| `inber/conversation/summarize*.go` | History summarization | Library: `llm-bridge/assembly/summarize` |
| `inber/conversation/staged.go`, `stash.go` | Compaction staging | Library: `llm-bridge/assembly/stage` |
| `inber/conversation/repair.go` | Tool-call repair | Library: `llm-bridge/assembly/repair` |
| `inber/conversation/extract*.go` | Reference extraction | Library: `llm-bridge/assembly/refs` |
| `inber/memory/*` | Vector recall | **Stays in memory-store** — context library calls it as a service |
| `inber/engine/build_*.go` | Tool wiring | **Stays in inber** — runtime concern |
| `inber/engine/turn_execute.go` | API call dispatch | **Stays in inber** — runtime concern |
| `inber/engine/failover.go` | Fallback chains | **Stays in inber** — runtime concern |

The split is: anything that **shapes the prompt** moves out into a library. Anything that **executes** the prompt (calls the model, dispatches tools, handles failover) stays in inber.

## Where the new library lives — decided 2026-05-10

Folds into `~/repos/llm-bridge/assembly/`. Renamed from `context/` to avoid overloading "context" (which already refers to /files-managed static prompts and to memory recall — too many meanings).

llm-bridge is already imported by every harness wrapper and the server; adding `assembly/` doesn't add deps, just makes existing imports do more. If it grows large enough to feel out of place, revisit by extracting at that point — but not pre-emptively.

## Surface the library exposes

Caller-facing API (sketch, not final):

```go
package context

// Assembler turns agent identity + history + recall + tool defs
// into a finished bridge-canonical Conversation, ready to hand to a harness.
type Assembler struct {
    AgentStore   AgentStoreClient
    MemoryStore  MemoryStoreClient
    ToolStore    ToolStoreClient
    Budget       Budget    // token budgets for sections
    Refs         RefResolver
    Summarizer   Summarizer
}

// Assemble builds the next turn's Conversation from current session state.
func (a *Assembler) Assemble(ctx context.Context, in TurnInput) (msg.Conversation, AssembleStats, error)

type TurnInput struct {
    SessionID   string
    AgentID     string
    History     []msg.Message       // prior turns
    UserMessage msg.Message         // the new user turn
    HarnessHints HarnessHints       // e.g. "harness owns prefix; only emit user msg"
}

type HarnessHints struct {
    OwnsSystemPrompt   bool   // CC subprocess: true. Inber direct-SDK: false.
    OwnsToolDefs       bool   // Same split.
    OwnsHistoryRewrite bool   // CC: true (CC manages cache). Inber: false (we manage).
}
```

`HarnessHints` is the key abstraction that lets one library serve both the inber-runtime path (own everything) and the harness-subprocess path (only fill in user turns + appended identity). The library knows which sections it can omit when the harness owns that surface.

## What gets retired in bridge-server

- `internal/server/agents_context.go` — replaced by calls to the new library at session start and per-turn.
- `internal/server/bypass_inject.go` — context injection becomes a library call, not a server-local mechanism.
- The scattered helpers around system-prompt building.

bridge-server keeps:
- harness lifecycle (manager, runner, manifests).
- tool-routing and tool provisioning (per `TOOL-ROUTING.md`).
- session/instance/credential plumbing.
- the HTTP/SSE surface.

It just delegates "what does the prompt look like" to the library.

## What changes in inber

inber's runtime continues to be the full-control path. After migration:

- `inber/engine/turn_prepare.go` becomes a thin wrapper around `assembly.Assembler.Assemble`.
- `inber/conversation/*` → most of it deletes; the remaining bits are inber-runtime-specific (e.g. failover).
- `inber/memory/*` stays as-is (it's already a service via memory-store).
- inber's HTTP API (consumed by `llm-bridge-inber`) is unchanged — clients don't see the internal refactor.

The migration is internally invasive but externally invisible. Nothing breaks for users.

## Migration sequence

A long-running, multi-step migration. Sequencing to keep main green throughout:

### Step 0 — write tests in inber for the modules being moved

Inber today has `context_budget_test.go`, `summarize_test.go`, `repair_test.go`, etc. Audit coverage. If a module is going to be lifted with no test, write the test first (in inber) so we have a regression net.

### Step 1 — create `llm-bridge/assembly/` skeleton

- New directory, new package, no behavior yet.
- Define the public types (`TurnInput`, `Assembler`, `HarnessHints`, `Budget`, `AssembleStats`, etc.).
- Stubs for each method that just panic with "not implemented".
- llm-bridge ships this; inber and bridge-server import it but don't use it yet.

### Step 2 — port modules one at a time, leaf-first

Order (rough — adjust as deps reveal themselves):

1. `dedup` (leaf, pure function)
2. `prune` (leaf)
3. `refs` (extract.go, extract_*.go)
4. `summarize`
5. `stage` (staged.go, stash.go)
6. `repair`
7. `blueprint` (depends on the above)
8. `assemble` (depends on all)

For each module:
- Copy code from inber to llm-bridge/assembly/<module>.
- Adapt to use abstract interfaces (`MemoryStoreClient`) instead of inber-internal types where applicable.
- Port tests.
- Mark inber's copy as a deprecated thin wrapper that calls into the library.
- Run inber's full test suite — should pass with delegation.

This step takes most of the calendar time. Each module is a small PR.

### Step 3 — bridge-server adopts the library

- `internal/server/agents_context.go` rewritten to call `context.Assembler.Assemble`.
- `internal/server/bypass_inject.go` deleted; inject is a library call.
- Conformance tests verify behavior parity (or improvement) vs. the old adapter.

### Step 4 — inber removes its delegation wrappers

- The thin wrappers from step 2 disappear; inber calls the library directly.
- inber/conversation/ shrinks dramatically. inber/engine/ keeps only the runtime concerns.

### Step 5 — cleanup

- Delete dead code in inber.
- Update inber's docs (ARCHITECTURE.md, etc.).
- Update llm-bridge's README to advertise the new `context/` package.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Inber's modules have hidden coupling to inber-internal types | Step 0 + interface-first refactor in Step 2 surfaces this early. Each module gets an interface for the boundary. |
| Behavior regression in bridge-server | Conformance test pass before Step 3 cutover. Roll back per module if needed. |
| llm-bridge becomes too heavy | If `context/` grows beyond ~5kloc, revisit Option A (extract to its own repo). Threshold is a soft one. |
| Migration drags on, two implementations co-exist for months | Time-box each step. If a module stalls > 2 weeks, simplify scope (port less, keep more in inber temporarily). |
| memory-store API doesn't match what `Assembler` needs | Add the missing memory-store endpoints as part of the port; do not work around with inber-internal calls. |

## What this is *not*

- Not a rewrite. We are lifting working code and shaping its boundaries.
- Not a chance to add features. Pure relocation. Feature work in `context/` happens after the dust settles.
- Not blocked on the harness sub-mode work. The two efforts are independent and can run in parallel.

## Open questions

1. **Naming.** `llm-bridge/assembly` vs. `llm-bridge/context` vs. `prompt-build` vs. just `context`. Recommend `llm-bridge/context` (Option B above).
2. **Budget model.** inber today has a `Budget` struct with per-section token caps. Keep that shape, or generalize? Probably keep — it works.
3. **Reference resolution latency.** `refs` resolves at assembly time and may hit the network (memory-store, noteboard). Should the assembler support partial / cached resolution? Defer; current behavior is fine.
4. **Streaming of assembly.** Today `Assemble` is one shot. If bridge-server wants to stream "assembling…" events to the UI, we may need an event channel argument. Defer until UX needs it.
5. **Order of work vs. CLI tool surface work.** Both are large. They don't conflict. Recommend: start the CLI surface gap-fill in inber-cli in parallel, since that's small and concrete; start the context migration as a separate track with its own pace.
