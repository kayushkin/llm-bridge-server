# Findings: interrupt no-op, duplicate user message, and turn hijack

Date: 2026-07-27
Investigated by: agent session (chat-surface bug triage on session `bridge-ui Vim keys`, `br_1784050100695400516`)
Status: **diagnosed, not yet fixed.** Fix plan below. A gateway restart is required to ship the server half — do NOT restart `llm-bridge.service` unattended.

This file is duplicated onto the noteboard (search tag `chat-surface-bugs`) so agents in other repos — notably the the chat page agent in `~/repos/dash` on branch `the chat page-page`, which consumes `@kayushkin/bridge-ui` — can find it.

---

## TL;DR

Four defects, three root causes. All confirmed against the live event log
(`~/.llm-bridge/bridge.db`), not inferred from source.

| # | Symptom (user-visible) | Layer | Root cause | Confidence |
|---|---|---|---|---|
| 1 | Sent message shows up 2–3× live, normal after reload | bridge-ui (render) | Optimistic row patched to canonical key *after* the SSE copy already created a second row; the dedup guard is defeated by the same patch | Confirmed |
| 2 | Stop button "sometimes" does nothing; a queued reply appears and ignores the new message | server + bridge-ui | Interrupt handler 409s unless `state == "running"`, but a tool-in-flight is `tool_running`. Client never checks the response, fakes a paused UI | Confirmed |
| 3 | One prompt renders as two separate turns | server (manager) | Every `user_message` mints a fresh `turn_id`, including Claude Code's OTel echo of the prompt — so the echo opens a second turn and all the real work lands in it | Confirmed |
| 4 | (latent) turn/message continuity reset mid-turn | server (manager) | `EventSessionState` case clears turn tracking keyed off the deprecated `running` value | Low — redundant + fragile, no proven live corruption |

Cross-cutting: **the `SessionState` enum is fiction** (see §5). This is the trap
that hid bugs 2 and 4 and it must be understood before touching any of them.

---

## Evidence sources

- Live gateway: `llm-bridge` on `:8160`, harness procs `llm-bridge-claudecode`.
- Event log: `~/.llm-bridge/bridge.db` (1.4 GB, WAL). Read-only queries via
  `sqlite3 "file:...?mode=ro"`. `events` table: `id, session_id, type, message_id, harness_message_id, data (json)`.
- The subject session `br_1784050100695400516` ("bridge-ui Vim keys") reproduces
  all four. Raw event dump quoted inline below.
- Distinct `session_state` values ever emitted, whole DB:

  ```
  tool_running        4547
  running             2665
  idle                2539
  error                966
  awaiting_permission  626
  completed            391
  awaiting_user        316
  ```

  Note what is **absent**: `model_generating`, `compacting`, `starting`,
  `paused`. Claude Code never adopted the granular vocabulary. This is the whole
  ballgame — see §5.

---

## Bug 1 — duplicate user message on the live path

**Symptom.** In the turns view the just-sent message appears twice; in the
thread view it appears as a standalone user row plus two separate turns, all
three the same text. Reload fixes it.

**Why reload is clean.** History is rebuilt from `/history` alone. Each prompt is
stored exactly twice — one harness/rollout copy (untagged) and one
`extensions.source = "otel"` copy — and `TurnsView` absorbs the OTel copy. No
optimistic row exists on reload, so it collapses to one.

**The live race (all file:line in `bridge-ui/src`).**

1. `useBridgeSession.ts:948` — `send()` appends an optimistic `LogRow` keyed by a
   temporary `clientId`, with **no `messageId`** and `events: []`.
2. Server side, `/send` (`llm-bridge-server/internal/server/sessions.go` ~L406)
   calls `harness.BroadcastEvent(&userEvent)` — which fans the `user_message` out
   to SSE subscribers (`manager.go:740`) — **before** it writes the JSON response
   (`sessions.go:423`, returns `{"message_id": userEvent.MessageID}`). The
   broadcast is strictly ordered before the reply, so on an already-open stream
   the SSE `user_message` **always** arrives while `await fetch` is still pending.
3. That SSE event hits `applyEventToRows` (`useBridgeSession.ts:259`). It looks
   for a row keyed `${message_id}_user_message`, does not find one (the optimistic
   row is still keyed `clientId`), and creates a **second row**.
4. `/send` finally resolves and `useBridgeSession.ts:977` patches the optimistic
   row's key to `${message_id}_user_message` **and sets `messageId`**. Now two
   rows carry the same key.

**Why the existing guard fails.** `TurnsView.tsx:96` has a guard written for
exactly this race: `if (!row.messageId && canonicalUserTexts.has(row.text)) continue`
— meant to drop the orphan optimistic row. But step 4 sets `messageId` on that
orphan, so `!row.messageId` is false and the guard never fires. The patch defeats
its own safety net. The orphan then has `events: []`, so `isOTelSourced` is false
and it's counted as a harness copy → `harnessUserTextCounts` = 2 → only one is
absorbed by the single OTel copy → two survive.

Thread view has **no dedup at all** (`Thread.tsx`, 69 lines) and groups by
`turn_id`, so the optimistic row (no turnId) renders standalone and the two real
copies render as two turn groups.

**Fix.** Reconcile in the `/send` response instead of blindly re-keying: if the
canonical row already arrived via SSE, retire the optimistic one.

```ts
if (body.message_id) {
  const newKey = `${body.message_id}_user_message`
  setLogRows(prev => {
    // BroadcastEvent precedes writeJSON, so the SSE copy is normally already here.
    if (prev.some(r => r.key === newKey && r.clientId !== clientId)) {
      return prev.filter(r => r.clientId !== clientId)   // drop the optimistic dupe
    }
    return prev.map(r =>
      r.clientId === clientId ? { ...r, messageId: body.message_id, key: newKey } : r)
  })
}
```

---

## Bug 2 — interrupt silently fails while a tool is running

**Symptom.** "Sometimes when I respond it instantly gives a full response as if
one was queued, and ignores my actual message." Correlates with having pressed
Stop.

**Root cause (server).** `handleInterruptSession` (`sessions.go:512`) guards:

```go
if sess.State != string(msg.SessionRunning) {   // sessions.go:520
    http.Error(w, "session not running", http.StatusConflict)  // 409
    return
}
```

`SessionRunning == "running"` is the state Claude Code emits **while the model
generates**. While a **tool is in flight** the state is `tool_running`. So the
guard passes during generation and **409s during tool execution** — which is the
most common moment a user hits Stop. That is the "sometimes."

**Root cause (client).** `interrupt()` (`useBridgeSession.ts:1005`) never checks
the response:

```ts
await fetchFn(`${basePath}/sessions/${id}/interrupt`, { method: 'POST' })
markInterrupted(id); markLastAssistantDone(); setActivity({ kind: 'idle' })
```

`fetch` does not throw on 4xx, so the 409 sails through and the UI
unconditionally shows a paused/idle session. **The UI says stopped; nothing
stopped.** The prior turn keeps running. When the user types the next message,
Claude Code (stream-json input) queues it behind the still-running turn, and that
turn's finished answer surfaces at the moment of send — looking like a
pre-queued reply that ignores the new prompt.

*(Partially characterised: `send()` also resets `lastEventId` to `undefined` and
reconnects SSE — `useBridgeSession.ts:984` — which replays the whole current turn
via `store.ListCurrentTurnEventsWithIDs`; this likely adds to the burst. Not
fully pinned down.)*

**The design error, not just the value.** The guard asks the *denormalised state
cache* a question the *live process registry* already answers. `Manager.Stop`
(`manager.go:356`) already returns `"session not running"` when there is no
process:

```go
func (m *Manager) Stop(sessionID string) error {
    proc := m.Get(sessionID)                       // m.processes[sessionID]
    if proc == nil { return fmt.Errorf("session not running: %s", sessionID) }
    return proc.Interrupt()
}
```

**Fix (server).** Delete the state check entirely; let `Stop()`'s error be the
409. Interrupting a live-but-idle process is a harmless no-op (Claude Code just
acks the control_request).

```go
sess, err := s.store.GetSession(bridgeID)
if err != nil { http.Error(w, "session not found", http.StatusNotFound); return }
if err := s.harness.Stop(bridgeID); err != nil {
    http.Error(w, err.Error(), http.StatusConflict); return
}
```

**Fix (client).** Check `res.ok`; surface a failed interrupt instead of faking a
paused UI (fail loud, per repo CLAUDE.md).

**Same shape next door.** `handleResumeSession` (`sessions.go:547`) gates on
`sess.State != SessionIdle` — same cache-as-authority mistake, and it collides
with the `SessionPaused` decision below.

---

## Bug 3 — the OTel echo hijacks the turn

**Symptom.** One prompt renders as two separate assistant turns.

**Root cause.** `case msg.EventUserMessage` (`manager.go:183`) mints a fresh
`turn_id` for **any** user_message:

```go
case msg.EventUserMessage:
    ...
    st.turnID = ids.NewTurnID()   // manager.go:193 — unconditional
```

Claude Code reports every prompt twice: the harness/rollout copy and, ~1s later,
an OTel `user_prompt` copy tagged `extensions.source = "otel"`. The manager treats
the echo as a brand-new prompt and opens a **second** turn. The real work lands in
the echo's turn; the turn the actual prompt opened is orphaned.

**Evidence** (`br_1784050100695400516`, ascending id):

```
482465 user_message   turn=…HYCGC0   msg=…XZNYZH   <- prompt via /send opens HYCGC0
482466 session_state   tool_running   turn=…HYCGC0
482469 user_message    turn=…C69B0M   msg=…HKSKYA   <- OTel echo opens a NEW turn C69B0M
482488…482741 tool_call turn=…C69B0M                <- all work attributed to the echo's turn
482754 result          turn=…C69B0M
482755 session_state   idle           turn=…C69B0M
482757 turn_complete    turn=…C69B0M
```

**Consequence for the render edge.** `TurnsView.tsx:30-41` documents the distinct
`message_id` *and* `turn_id` as inherent Claude Code behavior. **It is not — we
mint the second turn_id.** Fixing this at the manager largely removes the need for
the render-edge turn-splitting workaround.

**Fix (needs sign-off — see §7).** The manager must not open a new turn for an
OTel echo of the currently-open prompt. It cannot simply drop OTel user_messages:
in **PTY mode** keystrokes never pass through `/send`, so the OTel copy is the
*only* record of what the user typed. Proposed rule: *a `source=otel`
user_message whose text matches the prompt that opened the currently-open turn is
an echo (attach, do not open a turn); otherwise it opens one.*

---

## Bug 4 — redundant mid-turn clear (low confidence)

`case msg.EventSessionState` (`manager.go:233`) clears `bridgeMsgID /
harnessMsgID / clientRequestID / turnID` whenever `ev.State.State != SessionRunning`.

- The turn is already cleared correctly at the real boundary in
  `EventResult, EventError` (`manager.go:219`) and re-opened in `EventUserMessage`.
  This case is a redundant backstop.
- Its condition keys off the deprecated `running` value: it does nothing on
  `running` events and fires on `tool_running` / `awaiting_permission` / `idle`.
- I could **not** demonstrate live corruption from it — in the subject session all
  tool_calls within a turn kept the same turn_id. Recommend deleting it, but gate
  the deletion behind a manager unit test rather than asserting harm.

---

## §5 — the `SessionState` enum is fiction (read before touching §2/§4)

`llm-bridge/msg/provider.go` deprecates `SessionRunning = "running"` in favor of
granular states (`model_generating`, `tool_running`, `compacting`, `starting`,
`paused`, …). **That migration was only ever done on the enum, never on the
harnesses.** The live DB proves it: `running` is emitted 2,665 times and
`model_generating` / `compacting` / `starting` / `paused` **zero** times.

Implications:
- `running` is **not dead**. Do **not** delete it (an earlier draft plan did — it
  was wrong). It is the live "model generating" state.
- Any code comparing `== / != SessionRunning` is comparing against a value that
  means only *half* of "busy". That is precisely how bug 2 hid.
- The mock harness (`cmd/mock-harness/main.go`) emits only `running` at all three
  transitions — it never emits `tool_running`, so the interrupt bug's failing
  state is **unreachable in the e2e-smoke tier**. The test suite encodes the same
  false premise as the bug. **Fix the mock first** (emit `tool_running`), watch
  the interrupt test go red, then fix the handler.

The enum being a lie is a real problem but a **bigger** call — either finish the
migration (every harness emits granular states, touches all bridge repos +
conformance) or un-deprecate `running` and delete the states nothing emits. Keep
this OUT of the fix branches; flag for a separate decision.

### DECIDED 2026-08-04: finish the migration, Claude Code first. SHIPPED.

The decision landed on finishing it, and the shape turned out cheaper than this
section feared — because by then the server derived state centrally
(`internal/harness/derivation.go`), so no harness had to change at all. Harness-
emitted `EventSessionState` is dropped at intake; the derivation is the only
writer. "Migrating Claude Code" meant teaching that one state machine the
distinctions it was collapsing:

- **`model_generating` vs `tool_running`.** `EventUserMessage` answered
  `tool_running` for the whole turn, which is why `model_generating` was emitted
  zero times. A turn now opens in `model_generating` (no tool has been called and
  none may ever be), `EventToolCall` moves it to `tool_running`, and the LAST
  `EventToolResult` draining `activeTools` moves it back — the model reads the
  result and keeps going.
- **`compacting`.** Derived from Claude Code's own signals: `compact_ack` opens
  it, `compact_boundary` closes it back to whatever the turn was doing
  (`preCompactState`). An AUTOMATIC compaction emits only the boundary, so the
  boundary tolerates never having seen an ack and does not transition.
- **`starting`.** The spawn sites wrote `running` directly
  (`manager.go` ×2, `sessions.go` ×3, `mode_switch.go`). They now write
  `starting`, which is what the enum says that moment is.

**Consumers fixed in the same pass**, all three named in this doc:
`health.go` counted one string and now sums `msg.ActiveSessionStates()`;
`renamer.go` listed `running`/`idle` literally and now asks `IsActive()`;
`mode_switch.go`'s busy gate listed two states and missed `compacting`,
`starting` and the legacy `running` that nearly every live row carried.

`running` is NOT deleted. 2,665 historical rows hold it, `IsActive()` and
`projectServerSessionState` still read it, and nothing writes it any more.

---

## Secondary observations (same cache-as-authority class)

Each independently reaches for the state string to answer a question the process
registry owns:

- `health.go:346` — `counts.Running` via `ListSessionsByState("running")`;
  under-reports because it misses `tool_running` sessions. Should count live
  processes.
- `renamer.go:194` — `renamerStillAlive` returns true only for `running`/`idle`;
  a renamer actively in `tool_running` reads as dead. Should be
  `s.harness.Get(id) != nil`.

Both fold naturally into a single "is this session active" authority on the
manager during the restructure (branch 2, §8).

---

## §6 — What the chat-page / bridge-ui-consumer agent must know

`dash` (on its chat-page branch at the time) consumes `@kayushkin/bridge-ui`. The render-edge
bugs (1 and the render half of 3) live in the library, not in dash. Things to
carry into a new chat surface:

1. **Do not trust adjacency for dual-emit dedup.** The OTel copy of a prompt can
   arrive *after* the assistant reply (OTel exporter batches ~1s), so it is not
   adjacent to the prompt it duplicates. Dedup must be source+count based
   (`extensions.source == "otel"`), never positional. See `TurnsView.tsx:64-111`.
2. **The dedup currently lives ONLY in `TurnsView`.** `Thread.tsx` renders the
   OTel copy unconditionally. A v2 surface that renders from the same `LogRow[]`
   will show prompts twice unless it applies the same dedup. The right end state
   is one shared selector both views consume (branch 2).
3. **Optimistic-send reconciliation is required** (bug 1 fix). Any send path that
   appends an optimistic row must reconcile against the SSE copy, which normally
   arrives first.
4. **Interrupt must check `res.ok`.** A 409 from interrupt means "nothing was
   stopped" — the UI must reflect that, not fake idle.
5. **`turn_id` is not currently reliable as a turn key** until bug 3 is fixed at
   the manager (one logical prompt can span two turn_ids). After the server fix it
   becomes reliable; a v2 surface keying on turn_id should land *after* the server
   fix or carry the same OTel-turn merge.

---

## §7 — Open decisions (need user sign-off before coding)

- **Turn-hijack rule (bug 3):** confirm the "otel echo of the currently-open
  prompt attaches instead of opening a turn" semantics before it goes in the
  manager.
- **`SessionPaused` on interrupt:** ✅ **DECIDED 2026-08-04 — yes. SHIPPED.**
  `handleInterruptSession` writes `SessionPaused`, and writes it through
  `Manager.ForceSessionState` rather than straight to the store. That routing is
  the substance of the change: only `derive()` broadcasts, so a store write left
  the derivation still holding the pre-interrupt state AND left every SSE
  subscriber unaware. Learning about an interrupt by refetching is exactly why
  the client kept its own copy.

  bridge-ui's localStorage hack is **deleted** — `INTERRUPTED_KEY`,
  `loadInterruptedIds`/`saveInterruptedIds`, the `interruptedIds` state and ref,
  `markInterrupted`/`unmarkInterrupted`, the pruning effect and all nine call
  sites. `deriveSessionUIState` went with them: with no client layer left it was
  a wrapper around `projectServerSessionState`.

  ⚠️ **One clause of this bullet was wrong and did not ship: "forces
  `handleResumeSession` to accept `paused`".** It does not, and must not.
  `Manager.Stop` calls `proc.Interrupt()` and leaves the process REGISTERED, so a
  paused session has a live harness and resume 409s on the process registry —
  correctly. Paused means the user stopped the turn, not that the harness is
  gone. A paused session needs no resume: sending the next message continues it.
  `chat-core`'s `RESUMABLE_STATES` carried a note predicting `paused` would join
  it; that note has been corrected in place rather than acted on.
- **Enum migration (§5):** ✅ **DECIDED 2026-08-04 — finish it, Claude Code
  first. SHIPPED.** See the DECIDED block in §5.

---

## §8 — Planned work

**Branch 1 (correctness, ship first):**
- `mock-harness` emits `tool_running` (makes the interrupt bug testable).
- Interrupt: delete state check, let `Stop()` error be the 409 (`sessions.go`).
- Resume: same treatment (`sessions.go:547`).
- Client `interrupt()` checks `res.ok` and surfaces failure (`useBridgeSession.ts`).
- Turn-hijack fix in `manager.go` EventUserMessage (pending §7 sign-off).
- Delete redundant `EventSessionState` clear (`manager.go:233`) behind a unit test.
- `SessionPaused` on interrupt + drop the localStorage hack (pending §7 sign-off).
- Duplicate-message reconciliation (`useBridgeSession.ts` send path).
- **Does NOT include the gateway restart.** Build + test + push only; the user
  runs `systemctl restart llm-bridge.service`.

**Branch 2 (restructure, after branch 1 verified live):**
- Single "is this session active" authority on the manager; `health` + `renamer` +
  interrupt/resume consume it; the `state` column goes back to being a display
  cache nobody decides on.
- Extract the pure `LogRow` reducer (`applyEventToRows` / `groupKeyFor` /
  `applyDelta`) out of the 1,235-line `useBridgeSession.ts` into its own testable
  module; add a regression test for the bug-1 race.
- Hoist the dual-emit dedup into one shared selector both `TurnsView` and `Thread`
  (and dash's chat page) consume.

---

## Repo / branch state at time of writing

- `llm-bridge` and `llm-bridge-server`: on branch `refactor/session-lineage-naming`
  (NOT main), tracking `origin/refactor/session-lineage-naming`. Last commits
  2026-07-13/14. Treat as an in-progress branch — confirm with the user whether
  branch 1 stacks on it or forks from main.
- `bridge-ui`, `llm-bridge-aider`, `llm-bridge-nanoclaw`: on `main`, clean.
- Stray untracked files in `llm-bridge-server`: `patch_17e.py`, `patch_c75.py`
  (June noteboard-mutation scripts, not part of this work — left untouched).
- **Deploy caveat:** the server fixes require restarting the live gateway every
  session on the box routes through. Per standing rule, that restart is NOT done
  unattended.
