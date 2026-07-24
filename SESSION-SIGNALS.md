# Session Signals

One canonical **signal** record for anything a session surfaces to the human, behind a
single frontend interface. A signal is one of two **kinds**:

- **`question`** — needs an answer (pick an option, type freeform, or both).
- **`notification`** — an FYI that needs at most an acknowledgement (no answer expected).

Kind is **orthogonal to session type**. Herald, interactive, and autonomous sessions can
each raise either kind. Herald is a *delivery channel*, not a question — a herald relay may
be "here's the daily summary" (notification) just as easily as "which option do you want?"
(question). An ordinary interactive session mid-task can equally raise either: "should I
proceed?" (question) or "heads up, the migration finished" (notification).

This is **not greenfield**. The pieces exist and are split:

- `AskUserQuestion` already parks as a pending hook (`HookSourceUserInput`) with a
  structured payload and resolves via `POST /sessions/{id}/hooks/{request_id}/resolve`.
- Free-text questions are detected by `looksLikeQuestion` in
  `internal/harness/derivation.go`, which sets `SessionAwaitingUser` — but produces **no
  structured record** (just a state flag) and detects **only questions**, not the "I've
  finished / heads up" notification case.
- The frontend renders `awaiting_user` as 🔔 in `SessionList` with a "Needs you" inbox, and
  (as of the chat linker) can render a session reference chip anywhere in a message body.

This doc merges those into one record + one interface, generalized across kind and session
type.

> Read after [`HARNESS-LAYER.md`](./HARNESS-LAYER.md) (session state derivation) and
> alongside the chat **reference-chip linker** shipped in `bridge-ui`
> (`src/components/chat/refChips/`), which is the surface a linked signal renders into.

---

## Foundation already shipped — the reference-chip linker

`bridge-ui` detects bridge session ids (`br_` / `herald-` / `autoworker-`) anywhere in a
chat message body, and cue-prefixed noteboard todo uuids (`todo`/`item`/`card` + uuid), and
renders each as a chip whose dropdown lazily fetches live detail. Files:
`refChips/remarkRefChips.ts` (mdast plugin → `<ref-chip>` node), `refChips/RefChip.tsx`
(dropdown), `refChips/refData.ts` (fetch). `noteboardBasePath` was added to `BridgeConfig`.

The session chip panel already renders a badge for `awaiting_user` / `awaiting_permission`.
**That badge slot is where the signal answer/ack UI mounts.** The linker is the delivery
surface; the signal record below is the payload.

---

## The record — the single source of truth

Owned by **llm-bridge-server**, which already owns sessions, the `awaiting_user` state, and
the parked-hook lifecycle.

```
signal {
  id            string
  session_id    string
  session_type  "interactive"|"autonomous"|"herald"   // metadata, NOT a discriminator
  kind          "question"|"notification"
  source        "tool"|"derived"
  request_id    string?         // tool path only — the parked hook's id
  title         string          // the question, or the notification headline
  body          string?         // optional detail for a notification
  options       []{ label, value }   // questions only; suggested/pre-baked answers
  allow_freeform bool           // questions: whether an editable text answer is accepted
  answer        { option?, text? }?  // questions: picked option AND/OR freeform edit
  severity      "info"|"warn"?  // notifications only
  state         "open"|"answered"|"acknowledged"|"dismissed"
  linked_todo_id string?        // propagation to todos
  created_at, resolved_at
}
```

### Blocking vs non-blocking — the key behavioral split

- A **question** blocks: the session sits at `awaiting_user` until answered. This is the
  existing 🔔 "Needs you" behavior.
- A **notification** does **not** block: the session stays `idle`/continues. It surfaces in
  the inbox as an unread FYI and clears on acknowledgement. A worker that emits a
  notification keeps going; it is not parked.

This is why kind can't be inferred from session state alone — a notification leaves state
untouched. The record carries the kind explicitly.

### Two producers, one shape

- **`source:"tool"`** — a structured tool call parks a signal.
  - `AskUserQuestion` → `kind:"question"` (options from the tool input).
  - A notify-style tool/hook → `kind:"notification"` (non-blocking; auto-allowed even for
    unattended sessions, since nothing is being asked).
- **`source:"derived"`** — a **cheap-model pass** on a turn-end classifies the assistant's
  final text into `question` | `notification` | `neither`, and on the first two writes a
  signal in the same shape. This generalizes today's `looksLikeQuestion` (which only sees
  questions) into a kind-aware classifier — the upgrade `derivation.go` already anticipates.
  Only a derived `question` sets `awaiting_user`; a derived `notification` surfaces without
  blocking.

### One consumer — the unified interface

A single `bridge-ui` component renders a `signal` by kind:

- **question** → the prompt, `options` as buttons, and (when `allow_freeform`) an editable
  text box.
- **notification** → the headline + optional body, a **Dismiss/Acknowledge** action, and an
  optional freeform reply (routes to `/send` if the user wants to respond, but not required).

It mounts in **three** places, all reading the same record:

1. Inline in the raising session's own chat.
2. The "Needs you" inbox (`SessionList`) — questions and unacknowledged notifications.
3. **Inside the RefChip session panel** — open session A's chip while working in session B
   and answer/ack A's signal there. This is the cross-session answer, needing no new routing
   primitive beyond the record + the resolve verbs below.

### Resolve — per kind and source

Resolving flips `state`, writes `answer`/`resolved_at`, and (for questions) clears
`awaiting_user`:

- **question, `source:"tool"`** → existing `POST /sessions/{id}/hooks/{request_id}/resolve`
  with an allow verdict carrying `{option, text}` in `UpdatedInput`.
- **question, `source:"derived"`** → `POST /sessions/{id}/send`, injecting the option and/or
  freeform text as the next user message.
- **notification** → acknowledge/dismiss marks the record `acknowledged`/`dismissed`; an
  optional freeform reply routes to `/send`. No blocking state to clear.

### Propagate to todos (falls out)

Set `linked_todo_id` when the raising session is linked to a todo (the classifier already
records `session=<id>` on classified todos; kanban entity-links sessions to cards). A todo
with an `open` linked signal shows a badge; answering/acking from the todo view routes
through the same resolve verb.

---

## Open decisions (recommendation in **bold**)

1. **Derived-pass trigger, scope & cost.** A Haiku classify call on turn-ends. **Recommend:
   run it on every turn-end that isn't already a clean tool-driven resolution, classify into
   question | notification | neither, and only emit a signal for the first two** — with a
   per-harness opt-out. Note this widens the trigger beyond today's `looksLikeQuestion` (to
   also catch notifications), so cost is per-turn, not per-question; if that's too broad,
   fall back to: questions on `looksLikeQuestion` turns only, notifications opt-in.
2. **Unattended sessions (autonomous/herald).** Today `AskUserQuestion` is auto-*denied* for
   unattended sessions (`permission_prehook.go:95`). Split by kind: **notifications from any
   session type always surface non-blocking (never denied)**; **questions from herald should
   park (herald exists to relay a human ask)**; questions from fully-autonomous workers with
   no human attached may still deny/continue — confirm which.
3. **Restart gate.** New table + endpoints + derivation and prehook changes are gateway
   work. Standing rule: do all the code (branch/build/verify/push) but **do not restart the
   live gateway unattended** — the user does that.

---

## Sequenced implementation (when greenlit)

- **P1 — record + read API.** `signal` table in the session store; `GET
  /sessions/{id}/signals` and `GET /signals?state=open`. Backfill the tool path to write a
  `signal` row when `AskUserQuestion` parks (source of truth, no behavior change yet).
- **P2 — unified frontend component.** One `SignalCard` in `bridge-ui` handling both kinds;
  mount in chat, inbox, and the RefChip session panel. Resolve via the verbs above.
- **P3 — derived pass.** Kind-aware cheap-model classifier on turn-ends → `signal` row.
  Decision 1.
- **P4 — notify tool/hook + unattended handling.** A structured notification producer; split
  park/deny/surface by kind. Decision 2.
- **P5 — todo propagation.** `linked_todo_id` + todo-view badge + resolve.

The frontend surface (linker chips + badge slot) is already live, so P1–P2 are the shortest
path to a visible unified inbox; P3–P5 layer on without reworking it.
