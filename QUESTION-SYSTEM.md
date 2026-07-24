# Question System

One canonical **question** record that unifies the two ways a session asks the human
something — a structured tool call (`AskUserQuestion`) and a free-text turn that merely
*reads* as a question — behind a single frontend interface. Any question, whatever its
source, can be answered with a pre-baked option, a freeform edit, or both, from the asking
session, the "Needs you" inbox, or a reference chip in another session.

This is **not greenfield**. The pieces already exist and are split:

- `AskUserQuestion` already parks as a pending hook (`HookSourceUserInput`) with a
  structured payload and resolves via `POST /sessions/{id}/hooks/{request_id}/resolve`.
- Free-text questions are detected by `looksLikeQuestion` in
  `internal/harness/derivation.go`, which sets `SessionAwaitingUser` — but produces **no
  structured record**, just a state flag. Its own comment already flags the planned
  upgrade to a cheap-model classifier.
- The frontend already renders the `awaiting_user` "?" as a 🔔 in `SessionList` and a
  "Needs you" inbox, and (as of the chat linker) can render a session reference chip
  anywhere in a message body.

This doc merges those into one record + one interface.

> Read after [`HARNESS-LAYER.md`](./HARNESS-LAYER.md) (session state derivation) and
> alongside the chat **reference-chip linker** shipped in `bridge-ui`
> (`src/components/chat/refChips/`), which is the surface a linked question renders into.

---

## Foundation already shipped — the reference-chip linker

`bridge-ui` detects bridge session ids (`br_` / `herald-` / `autoworker-`) anywhere in a
chat message body, and cue-prefixed noteboard todo uuids (`todo`/`item`/`card` + uuid),
and renders each as an interactive chip. Clicking opens a dropdown that lazily fetches live
detail: session state/type/model/harness/cost/updated, or todo title/status/tags/priority/
due with held/deleted badges. Files: `refChips/remarkRefChips.ts` (mdast plugin →
`<ref-chip>` node), `refChips/RefChip.tsx` (dropdown), `refChips/refData.ts` (fetch).
`noteboardBasePath` was added to `BridgeConfig`.

The session chip panel already shows a **`question` / `approval` badge** when a session is
`awaiting_user` / `awaiting_permission`. **That badge slot is where the unified question
answer UI mounts.** The linker is the delivery surface; the question record below is the
payload.

---

## The record — the single source of truth

Owned by **llm-bridge-server**, which already owns sessions, the `awaiting_user` state, and
the parked-hook lifecycle. Generalize the pending hook into a first-class `question`:

```
question {
  id            string
  session_id    string          // the asking session
  source        "tool"|"derived"
  request_id    string?         // tool path only — the parked hook's id
  prompt        string          // the question text
  options       []{ label, value }   // suggested / pre-baked answers (may be empty)
  allow_freeform bool           // whether an editable text answer is accepted
  answer        { option?, text? }?  // picked option AND/OR freeform edit
  state         "open"|"answered"|"dismissed"
  linked_todo_id string?        // for propagation to todos (Phase 3)
  created_at, answered_at
}
```

### Two producers, one shape

- **`source:"tool"`** — created when `AskUserQuestion` parks
  (`permission_prehook.go` → `parkPrehook(..., HookSourceUserInput)`). Already structured;
  `options` come straight from the tool input, `allow_freeform` per the tool's schema.
- **`source:"derived"`** — created by a **cheap-model pass** when a turn ends and
  `looksLikeQuestion` fires. The pass reads the assistant's final text and emits
  `{prompt, options[]}` in the same shape (`allow_freeform` defaults true). This is the
  "LLM pass over an existing output that makes a real question" — the upgrade
  `derivation.go` already anticipates. It replaces the raw heuristic's *meaning* while
  still setting `awaiting_user` for backward compatibility with the inbox.

### One consumer — the unified interface

A single `bridge-ui` component renders a `question` regardless of source: the prompt, the
`options` as buttons, and (when `allow_freeform`) an always-available editable text box.
It mounts in **three** places, all reading the same record:

1. Inline in the asking session's own chat, where the turn ended.
2. The "Needs you" inbox (`SessionList`).
3. **Inside the RefChip session panel** — open session A's chip while working in session B
   and answer A's question there. This is "answer from a different session," and it needs
   no new answer-routing primitive beyond the record + the resolve verbs below.

### One verb, two transports

Answering flips the record to `answered`, writes `answer`, and clears `awaiting_user`:

- **`source:"tool"`** → existing `POST /sessions/{id}/hooks/{request_id}/resolve` with an
  allow verdict carrying `{option, text}` in `UpdatedInput`.
- **`source:"derived"`** → `POST /sessions/{id}/send`, injecting the chosen option and/or
  freeform text as the next user message.

### Phase 3 — propagate to todos (falls out)

Set `linked_todo_id` when the asking session is linked to a todo — the classifier already
records `session=<id>` on classified todos and kanban entity-links sessions to cards. Then:
a todo with an `open` linked question shows a badge (reuse the chip badge), and answering
from the todo view routes through the same resolve verb. No new machinery.

---

## Open decisions (recommendation in **bold**)

1. **Derived-pass trigger & cost.** A Haiku call per `awaiting_user` turn-end (not every
   turn). **Recommend: on by default, gated strictly to turns where `looksLikeQuestion`
   already fired** (so cost tracks question-ish turns, ~cents/day), with a per-harness
   opt-out. Alternative: opt-in per session.
2. **Structured questions for unattended/herald sessions.** Today `AskUserQuestion` is
   auto-*denied* for `autonomous`/`herald` (`permission_prehook.go:95`). **Recommend:
   special-case herald so it parks a structured question instead of denying** (the change a
   memory note already flagged) — herald exists precisely to relay a human question.
   Autonomous workers stay denied. Alternative: keep herald on the derived-pass path only.
3. **Restart gate.** New table + endpoints + the derivation and prehook changes are real
   gateway work. Standing rule: do all the code (branch/build/verify/push) but **do not
   restart the live gateway unattended** — the user does that.

---

## Sequenced implementation (when greenlit)

- **P1 — record + read API.** `question` table in the session store; `GET
  /sessions/{id}/questions` and `GET /questions?state=open`. Backfill the tool path to
  write a `question` row when it parks (source of truth, no behavior change yet).
- **P2 — unified frontend component.** One `QuestionCard` in `bridge-ui`; mount in chat,
  inbox, and the RefChip session panel. Freeform + options. Resolve via the two transports.
- **P3 — derived pass.** Cheap-model extraction on `awaiting_user` turn-ends → `question`
  row with `source:"derived"`. Decision 1.
- **P4 — herald structured asks.** Decision 2: park instead of deny for herald.
- **P5 — todo propagation.** `linked_todo_id` + todo-view badge + resolve. Phase 3.

The frontend surface (linker chips + badge slot) is already live, so P1–P2 are the
shortest path to a visible unified inbox; P3–P5 layer on without reworking it.
