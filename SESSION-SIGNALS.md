# Session Signals

One canonical **signal** record for anything a session surfaces to the human, behind a
single frontend interface. A signal is one of two **kinds**:

- **`question`** — needs an answer (pick an option, type freeform, or both).
- **`notification`** — an FYI that needs at most an acknowledgement (no answer expected).

Kind is **orthogonal to session type** — but *where a signal surfaces* is not. Herald and
interactive sessions raise either kind into the chat inbox. Herald is a *delivery channel*,
not a question — a herald relay may be "here's the daily summary" (notification) just as
easily as "which option do you want?" (question); an interactive session mid-task raises
either: "should I proceed?" (question) or "heads up, the migration finished" (notification).
Autonomous workers **do not ask interactive questions** (nobody is watching their chat), but
a genuine blocker or an FYI still needs to go somewhere — it surfaces on the worker's
**kanban card** for async review. So the record is unified; the `surface` field (below)
routes it. See "Where a signal surfaces."

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
  surface       "chat"|"kanban" // where it renders — a projection of attended-ness
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

### Where a signal surfaces — attended vs unattended

`surface` is derived from whether a human is watching the session's chat, not from kind:

- **Attended** (`interactive`, `herald`) → **`surface:"chat"`**: the "Needs you" inbox, the
  raising session's chat, and reference chips. Questions block at `awaiting_user`.
- **Unattended** (`autonomous` workers) → **`surface:"kanban"`**: no human is on that chat,
  so nothing routes there. **Autonomous workers do not ask interactive questions — the
  `AskUserQuestion` auto-deny stays.** A worker must not sit blocked waiting on a human. But
  a genuine blocker isn't lost in the deny: it is recorded as a signal on the worker's
  **kanban card** (the autoworker already opens an In-Progress card; kanban-curator manages
  its state), moving the card to a blocked/needs-review state carrying the blocker text.
  Resolution is **async and multi-party** — the orchestrator (task-completion-loop
  dispatcher, most likely), the user, or another agent reviews the board and answers; the
  dispatcher already revives sessions, so the answer routes back to a revived worker via
  `/send`. Notifications from a worker likewise land on the card as activity, not a chat
  park.

The record stays single-source-of-truth; `surface` just says which consumer renders it.

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

For `surface:"chat"` it mounts in **three** places, all reading the same record:

1. Inline in the raising session's own chat.
2. The "Needs you" inbox (`SessionList`) — questions and unacknowledged notifications.
3. **Inside the RefChip session panel** — open session A's chip while working in session B
   and answer/ack A's signal there. This is the cross-session answer, needing no new routing
   primitive beyond the record + the resolve verbs below.

For `surface:"kanban"` the same `SignalCard` renders on the worker's card (the kanban board
already mounts in dash + llmux via `BridgeKanban`), so the orchestrator/user/agent answers a
blocker where they already triage worker state.

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

1. **Derived-pass trigger, scope & cost.** *Resolved: on by default.* A Haiku classify call
   runs on every turn-end that isn't already a clean tool-driven resolution, classifying into
   question | notification | neither and emitting a signal for the first two. Keep a
   per-harness opt-out as an escape hatch. Cost is per-turn, not per-question (it now also
   catches notifications) — acceptable at Haiku pricing.
2. **Unattended sessions (autonomous workers).** *Resolved:* autonomous workers **do not ask
   interactive questions** — the `AskUserQuestion` auto-deny (`permission_prehook.go:95`)
   stays; a worker must not block on a human. Herald questions **park** (herald exists to
   relay a human ask). Notifications from any session type surface non-blocking, never
   denied. A worker's genuine blocker becomes a **`surface:"kanban"` signal on its card** for
   async orchestrator/user/agent review, not a chat park. Remaining sub-question: on a real
   blocker, should the worker **stop** and post the blocker, or **continue** past it and post
   for later review? (Default: stop — a blocker by definition can't be worked around.)
3. **Restart gate.** *Resolved: user handles the gateway deploy/restart* (deploy coming
   soon). I do all the code — branch/build/verify/push — and stop before the restart.

---

## Sequenced implementation (when greenlit)

- **P1 — record + read API. SHIPPED.** See "P1 as built" below.
- **P2 — unified frontend component.** One `SignalCard` in `bridge-ui` handling both kinds;
  mount in chat, inbox, and the RefChip session panel. Resolve via the verbs above.
- **P3 — derived pass.** Kind-aware cheap-model classifier on turn-ends → `signal` row.
  Decision 1. **Shipped — see "P3 as built" below.**
- **P4 — notify tool/hook + surface routing.** A structured notification producer; set
  `surface` from attended-ness. Unattended blockers → `surface:"kanban"` signal on the
  worker's card (move card to blocked/needs-review), resolvable by orchestrator/user/agent;
  the dispatcher revives the worker with the answer. Keep the autonomous `AskUserQuestion`
  deny. Decision 2.
- **P5 — todo propagation.** `linked_todo_id` + todo-view badge + resolve.

The frontend surface (linker chips + badge slot) is already live, so P1–P2 are the shortest
path to a visible unified inbox; P3–P5 layer on without reworking it.

---

## P1 as built

The record and its read API are live in code. Nothing about existing behavior
changed: a parked `AskUserQuestion` still parks, resolves and returns exactly as before,
and the rows are written alongside it.

**Type.** `msg.Signal` in `llm-bridge/msg/signal.go`, with `SignalKind`, `SignalSource`,
`SignalSurface`, `SignalState`, `SignalSeverity`, `SignalOption` and `SignalAnswer`.
TypeScript regenerated into `llm-bridge/ts/msg.ts`, so `bridge-ui` can build the P2
`SignalCard` against the same shape with no hand-written interface.

**Storage.** `signals` table in the session store (`internal/store/signals.go`), created by
the same idempotent `migrate()` every other table uses. `CreateSignal`, `GetSignal`,
`ListSignals(SignalFilter)`, `ListSignalsByRequestID`, `ResolveSignal`.

`ResolveSignal` only moves a row out of `open` — a duplicate resolve (two clicks, or a
stale resolve arriving after the real one) cannot overwrite the resolution that actually
happened.

**Read API.**

- `GET /sessions/{id}/signals` — one session's signals, newest first.
- `GET /signals` — the cross-session inbox. `?state=open` is what the "Needs you" inbox
  wants.
- Both take `state`, `kind`, `surface` and `limit`; `/signals` also takes `session_id`. An
  unrecognized enum value is a **400, not an empty list** — a typo that silently returns
  `[]` reads as "you have no signals", which is the one wrong answer this endpoint can give.

**Tool-path backfill.** `parkPrehook` records the signals once the park is genuinely
established (after the `awaiting_resolution` broadcast succeeds — recording earlier would
strand open rows on the broadcast-failure path, where the park is cancelled and no human
ever sees the question). `broadcastPrehookResolved` and `broadcastStaleResolution` close
them out, so every way a parked request can end also ends its signals: answered on allow,
dismissed on deny or on a park cancelled by a dead client, and dismissed on a stale resolve
after a harness restart.

Two details worth not re-deriving:

- **One signal per question, not per request.** `AskUserQuestion` carries an *array* of
  questions while the record holds a single title and option set. Flattening them into one
  row would lose questions, so each question gets its own row and they share `request_id`.
  That also makes the answer pairing trivial: the resolve payload is keyed by question text,
  which is exactly what each row's `title` holds.
- **`surface` is not `isUnattendedSession`.** They disagree on herald, deliberately.
  `isUnattendedSession` asks "can a parked ask be resolved on this turn?" — no for herald,
  since no human is attached during the relay. `signalSurfaceForSession` asks "where does
  the human eventually read this?" — the chat inbox, since no herald session has a kanban
  card. Only `autonomous` routes to kanban.

**Not yet true, and P1 does not pretend otherwise.** No signal is written for anything but
a parked `AskUserQuestion`: no derived pass (P3), no notification producer (P4), and no
autonomous worker signal, because an autonomous `AskUserQuestion` is still denied before it
can park. So `surface:"kanban"` is reachable in the type and the query but nothing mints one
yet — P4 is what starts. `linked_todo_id` is likewise carried and never set (P5).

---

## P2 as built (2026-07-31)

The card exists and mounts in all three chat surfaces. Branch: `bridge-ui`
`feat/session-signals-p2` (`a38e79c`, **pushed**). Nothing on the server changed.

**Not mergeable to `bridge-ui` main yet.** `msg.Signal` lives on `llm-bridge`'s
`feat/session-signal-record` branch, so a clean clone of bridge-ui main beside llm-bridge
main would not build — and `repo-node-guard` audits exactly that pairing nightly. Merge
after the record lands on llm-bridge main.

**What is there.**

| Piece | File |
|---|---|
| The card — one signal, by kind | `bridge-ui/src/components/chat/SignalCard.tsx` (`SignalCard`) |
| The unit that resolves — one parked request | same file (`SignalRequestCard`) |
| Read + resolve client, cross-surface refresh | `bridge-ui/src/components/chat/signalData.ts` |
| The two self-fetching surfaces | `bridge-ui/src/components/chat/SessionSignals.tsx` (`SessionSignals`, `SignalsInbox`) |
| Mount 1 — raising session's chat | `components/chat/Workspace.tsx`, beside `PendingPermissionsBanner` |
| Mount 2 — "Needs you" inbox | `components/chat/SessionList.tsx`, under the harness filter bar |
| Mount 3 — RefChip session panel | `components/chat/refChips/RefChip.tsx`, under the State row |

**Five things not to re-derive.**

1. **The card renders a signal; the REQUEST is what resolves.** One `AskUserQuestion` mints
   several rows sharing a `request_id`, and resolving that request answers all of them at
   once — so Submit stays disabled until every question in the request has an answer.
   Answering one row in isolation would resolve the whole request with the rest blank.
2. **Do not rebuild the tool input from the signal rows.** The resolve verb replaces the
   tool input *wholesale*, and the record carries no `multiSelect` and no option previews, so
   a rebuilt input silently downgrades the call. The client fetches the parked input from
   `GET /sessions/{id}/hooks/pending` and posts it back untouched under `answers`. If the
   request is no longer parked, that is an error the user sees — not a resolve posted into
   the void.
3. **The chat mount excludes request_ids the pending-hook banner is already showing.**
   Both render the same parked question, and the banner renders it better (it has the live
   tool input). What is left for the card in-session is what the banner cannot show: a
   signal whose park died with a harness restart, and — once P3 lands — derived ones.
4. **A 404 from the signals route hides every surface, silently.** It means this
   bridge-server predates the API, which is the state of the live gateway until P1 deploys.
   An error banner for that would be an error banner on every page, in three places.
   Reads therefore use `/signals?session_id=…` rather than `/sessions/{id}/signals`: the
   per-session route 404s for *both* "no signals route" and "no such session", and only the
   first should hide the UI.
5. **A resolve on one surface refetches every mounted surface.** There is no signal event
   on the SSE stream, so the resolve helpers announce the change in-process and each
   `useOpenChatSignals` refetches from the server. It is a refresh trigger, not a cache —
   the server stays the source of truth for what is open. Without it, answering in the
   sidebar inbox left an open RefChip panel offering a question that was already answered.

**Verified in a headless browser** against a stub bridge-server, not reasoned about: a
half-answered request keeps Submit disabled; a fully answered one posts the original
questions array with `multiSelect:true` intact plus `answers` keyed by title; resolving in
the inbox cleared the untouched panel too (checked curative — before the in-process
announce it stayed on screen); and with the route 404ing, neither surface renders anything.

**What P2 does not do.** Notifications render (headline, body, severity) but carry no
acknowledge action, because no HTTP verb moves a signal out of `open` on its own — only the
hook-resolve path does, and that is tool-question-only. `SignalCard` takes an
`onAcknowledge` prop and no surface passes one, so no button appears rather than a button
that does nothing. The verb arrives with P4. Likewise a signal with no `request_id`
(derived, P3) renders read-only: its resolve path is `POST /sessions/{id}/send`, which P3
adds. And `surface:"kanban"` has no mount yet — that is P4's, on the kanban card.

---

## P3 as built (2026-07-31)

The derived producer. A cheap-model pass over each turn-end sorts the assistant's final
text into `question` | `notification` | `neither` and writes a `source:"derived"` signal
for the first two. On by default (decision 1), with a per-harness opt-out.

| Piece | File |
|---|---|
| Classifier, skip rules, signal write, state reconciliation | `internal/server/signal_classifier.go` |
| Turn-end observer + bounded external state write | `internal/harness/manager.go` |
| `applyExternalState` / `currentState` | `internal/harness/derivation.go` |
| Answering a derived question | `internal/server/sessions.go` (`handleSendMessage`) |
| Config + escape hatch | `internal/config/config.go` |
| Client half | `bridge-ui` `feat/session-signals-p3` (`c35a7da`) |

Env: `LLMBRIDGE_SIGNAL_CLASSIFIER_MODEL` (default `claude-haiku-4-5`; **empty turns the
classifier off everywhere**), `LLMBRIDGE_SIGNAL_CLASSIFIER_OPT_OUT` (comma-separated
harness names — the escape hatch), `LLMBRIDGE_SIGNAL_CLASSIFIER_TIMEOUT` (20s),
`LLMBRIDGE_SIGNAL_CLASSIFIER_MAX_CHARS` (6000).

### The five things not to re-derive

**1. The classifier cannot live inside `derive()`, so `looksLikeQuestion` stays.**
`derive()` runs on the hot event path holding the state mutex; a model call there parks
the session's whole event stream behind an API round trip — the exact wedge the
turn-completion watchdog exists to break. So the heuristic keeps its job as the
*immediate provisional* answer (the 🔔 appears the instant the turn ends) and the
classifier is the *authoritative later* one, promoting a question the string match
missed and demoting one it called wrong. Demotion is the half that makes the feature
worth its cost: today a turn ending "does that make sense?" parks a session with nothing
to answer.

**2. A late verdict must be bounded, or it corrupts state.**
`applyExternalState` takes an `allowedFrom` set naming the states the verdict was formed
about, and drops the write for anything else — so a classifier that answers after the
user has already replied cannot flip a running turn back to `awaiting_user`. **An empty
`allowedFrom` is rejected, not read as "any state"**: the failure mode of an unbounded
default is silent state corruption, and a caller that forgets the bound should get no
write rather than an unbounded one. Verified curative — removing the bound fails two
tests.

**3. Nothing else closes a derived row, so two paths close it here.**
There is no signal-level resolve verb until P4. A derived **question** closes in the
`/send` handler, because sending a message *is* its resolve verb — done server-side, not
in the card, so a question answered from the CLI or by an orchestrator closes the same
way. Anything still open is **superseded** at the next turn-end: the assistant has
spoken again, so a free-text ask from a previous turn is answered or moot. Without
supersession every turn would leave another stale row in the inbox forever. At most one
derived row per session is ever open.

**4. The turn's tail is what gets classified, and a parked tool ask wins.**
Truncation keeps the **end** of a long turn — a question a human has to answer is always
last, so a head-first cap would classify the file dump above it. And when a structured
`AskUserQuestion` is already parked, the derived pass records nothing: that ask has a
real resolve verb behind it, and a derived row alongside it is a second, weaker copy of
the same demand on the user's attention. The store lookup **fails closed** — an
unreadable store must not become a licence to mint duplicates on top of a park we could
not see.

**5. `surface` routes; state describes.**
A derived question sets `awaiting_user` for every session type, autonomous included —
the state is an honest description of a session that ended its turn with an unanswered
question. Where that question *reaches a human* is `surface`'s job, and for an
autonomous worker that is the kanban card (P4), not a chat nobody is reading. Collapsing
the two would mean either lying about the session or losing the signal.

### Cost and the off switch

One Haiku call per turn-end, skipped for: an empty or under-12-character final message, a
renamer session (our own helper — its sign-off looks enough like a notification to mint
one every time), a turn that did not settle (errored, aborted, or already superseded by a
new turn), an opted-out harness, and a session with a tool ask already parked. Every
failure path — timeout, transport error, unparseable verdict, no credential — leaves the
session exactly as the heuristic left it and records nothing. A signal is an extra
surface on top of a turn that already completed; no failure to produce one may change how
the turn itself resolved.

---

## P5 as built (2026-07-31)

`linked_todo_id` is filled in, and a todo carrying an open signal says so on its card.

| Piece | File |
|---|---|
| The join, as a client | `internal/kanbanclient/client.go` (`LinkedTodoForSession`) |
| Resolved once per signal mint | `internal/server/signal_todo_link.go` |
| Stamped by the tool producer | `internal/server/signals.go` |
| Stamped by the derived producer | `internal/server/signal_classifier.go` |
| Read API: `?linked_todo_id=` | `internal/server/signals.go`, `internal/store/signals.go` |
| Deterministic link order | `kanban-store` `internal/db/db.go` (`ListCardsByEntity`) |
| The badge | `bridge-ui` `src/components/BridgeKanban.tsx` (`SignalBadge`) |

Env: `LLMBRIDGE_KANBAN_STORE_URL` (default `http://localhost:8305`; **empty turns the
lookup off** and every signal is minted unlinked).

### The join already existed, in one place

`card_links(card_id, entity_type='session', entity_ref=<bridge session id>)` in
kanban-store, read back as `GET /api/entities/session/{id}/cards`. **A card id IS a
noteboard item id** — kanban-store has no cards table, only placements and links against
the noteboard item — so those card ids are todo ids directly, with no second join.

Nothing else ties the two together. bridge-server's session record carries no todo field
of any kind, noteboard's item carries no session field, and the `session=<id>` lines the
autoworker and the classifier write into card bodies are prose, not a join. So the
lookup goes to the service that owns the link and asks it, rather than growing a column
somewhere to cache the answer.

### The five things not to re-derive

**1. Oldest link wins, and the ordering belongs to kanban-store.**
One session is often linked to several cards: the autoworker links its dispatch todo at
fire time, and the kanban classifier links a card per piece of work it later recognizes.
The record holds one `linked_todo_id`, so something has to choose. The choice is the
**oldest** link — a session's reason for existing is recorded before it does anything,
so the first link is the todo the session is *for* and every later one describes work it
has since done. That rule needs no label allowlist, and it reads the same for a chat
session as for a worker.

Making it reachable took a fix in kanban-store: `ListCardsByEntity` was
`SELECT DISTINCT card_id` with **no ORDER BY**, so the order was whatever SQLite found
convenient. It happens to be insertion order today and stops being it the moment the
planner picks the `(entity_type, entity_ref)` index. A caller that reads row 0 was
relying on unspecified behaviour. Now `GROUP BY card_id ORDER BY MIN(created_at),
card_id`. The order cannot diverge from insertion order through the HTTP API (created_at
is stamped at insert), which is why the curative test inserts rows directly with
out-of-step timestamps — through the API the old code passes.

**2. Resolved at mint time, and that has a consequence worth stating.**
The record is the source of truth for what was true when the signal was raised, and a
read-time join would call kanban-store once per inbox row. So the lookup runs once, when
the signal is minted.

The consequence: **a session that acquires its first link after raising a signal leaves
that signal unlinked forever.** That is the autoworker case working and the chat case
mostly not — the autoworker links its dispatch todo before the session starts, while the
kanban classifier links a chat session's work up to fifteen minutes later. This is not a
gap to paper over with a back-fill. An empty `linked_todo_id` is the truth about that
moment; claiming a link the signal was not raised under would be worse than claiming
none.

**3. A failed lookup costs the pointer, never the signal.**
kanban-store down, wedged, absent from config, or answering junk — every one logs and
returns `""`. A signal is an extra surface on top of work that already happened; losing
the todo pointer must not lose the signal. Verified curative: making the lookup fatal
fails the "still recorded" test three ways (500, garbage body, unreachable host).

**4. `?linked_todo_id=` with an empty value is a 400, not "everything".**
Same reasoning as the enum params: a todo view that meant to name its todo and named
nothing would otherwise receive every signal in the store and badge itself with another
todo's question. Omitting the parameter still means "don't narrow" — the two are
distinguished with `q.Has`, not `q.Get(…) != ""`.

**5. The board fetches once; a single-todo view narrows server-side.**
`useOpenSignalsByTodo` makes **one** `?state=open` request per board and groups the
result by todo, because the open set is small by construction (at most one derived row
per session, plus whatever tool asks are parked) and one request per card would be N
requests for a list the server can hand over whole. `fetchOpenSignalsForTodo` is the
other shape — `?linked_todo_id=` — for a view that already knows its one todo. Rows with
an empty `linked_todo_id` are dropped from the grouping: an unlinked signal belongs to no
todo, not to "the todo with the empty id".

Surface is deliberately **not** filtered for the badge. A todo is worked by chat sessions
and by autonomous workers alike, and the badge answers "does this work need me?", which
is true of a signal on either surface.

### What P5 does not do

The badge states the problem and does not offer to solve it: it names the leading signal
and its kind, and answering happens where a resolve verb exists — the chat, the inbox, or
the RefChip panel. An answer box on a board card would promise a resolution the board
cannot deliver, because **notifications still have no acknowledge verb** (P4's) and
`surface:"kanban"` still has no full card mount (also P4's). When P4 lands, the mount
goes in the card drawer and the badge stays what it is — the thing you can see without
opening anything.

dash's `/notes` page is a separate, dash-local React page that talks to noteboard
directly; it is not a bridge-ui surface and gets no badge here. `?linked_todo_id=` is the
query it would use.

---

## P4 part 1 as built (2026-08-01) — the signal-level resolve verb

`POST /signals/{id}/resolve`. The close verb for the resolutions that never reach the
raising session, and the reason `SignalCard`'s `onAcknowledge` prop can finally be passed
by a surface. Until it existed the only ways out of `open` were the parked-hook resolve
(tool questions) and a `/send` (derived questions), so **a notification stayed open
forever** — the gap P2, P3 and P5 each recorded and each left alone.

| Piece | File |
|---|---|
| Handler + the pure unpark rule | `internal/server/signals.go` (`handleResolveSignal`, `resolutionUnparksSession`) |
| Live-park lookup | `internal/server/parked_asks.go` (`isParked`) |
| Route | `internal/server/server.go` |
| Client half | `bridge-ui` `feat/session-signals-p4-ack` |

Body is `{"state": "acknowledged" | "dismissed"}`. The response is the stored row, re-read
after the write.

### The five things not to re-derive

**1. `answered` is refused, and that is the whole shape of the verb.**
An answer has to *reach the session*. The two paths that carry one —
`POST /sessions/{id}/hooks/{request_id}/resolve` and `POST /sessions/{id}/send` — already
close their own rows. Accepting `answered` here would record an answer the session never
received, which is worse than leaving the row open: a worker would read the todo as
handled. The 400 names both paths, because a caller that is refused needs its next move.

**2. Acknowledging is the notification verb, and a question cannot use it.**
A question nobody answered has not been handled. Letting it grade `acknowledged` — "seen"
— is exactly the collapse `feedback_status_enum_granularity` warns about, and the kanban
surface is where it would cost most: a card whose blocker reads as read. A question closes
by being answered, or by `dismissed`, which says out loud that no answer is coming.

**3. A tool signal whose request is STILL PARKED is a 409, not a close.**
The signal is a surface on top of the park; the park is the source of truth for whether
the session is blocked. Closing the surface out from under a live park hides the ask while
the harness keeps sitting on the channel — the session wedges with nothing on screen that
explains why. So the verb asks `parkedAsks.isParked` first and refuses, naming the hook
route, which closes both. Once the park is gone (harness restart, cancelled request) the
row is a leftover with nothing behind it and dismissing it is the only way to clear it —
that path stays open, and is tested.

`isParked` is read-only on purpose. A version that consumed the entry would turn every
409 check into a wedged session; there is a curative test for exactly that.

**4. A duplicate resolve is ordinary, and the rule that makes the first one win lives in
ONE place.** The same row renders in the chat, the inbox, the RefChip panel and the kanban
drawer, so two clicks are expected. `store.ResolveSignal` already updates only
`WHERE state='open'`, so the handler does **not** repeat that check — it writes, re-reads,
and returns what is stored. An earlier draft had the guard in both, and a sabotage run
showed the handler's copy changed no observable behaviour, so it was removed rather than
kept as decoration.

**5. The one thing a duplicate DOES cost is the unpark, so that check is where the
open-state test went.** A derived question parks its own session at `awaiting_user` when
the classifier mints it, so dismissing it walks the session back to idle via
`ApplyDerivedSessionState` bounded to `awaiting_user`. A second click landing later must
not write that state again: by then the session may be at `awaiting_user` on a **new**
question, which the `allowedFrom` bound cannot tell apart from the old one. Hence
`resolutionUnparksSession` requires the row to still be open. Nothing else needs the walk
— a derived notification already drove the session to idle at mint time, and a tool
signal's session state belongs to its parked hook.

`resolutionUnparksSession` is a pure function, deliberately: the alternative was a seam
for the harness manager, and the cross-product of source × kind × state is what actually
needed covering. The bounded write itself is tested in the derivation package.

### Verified

Eight sabotages, each one run, all eight caught: accept `answered`; acknowledge a
question; drop the parked-request guard; make `isParked` consume its entry; drop the
open-state check from the unpark rule; unpark on every dismissal; echo the request back
instead of re-reading the row; 200 instead of 404 on an unknown signal. Full
`internal/server` package green.

⚠️ **A `-run` filter is part of the sabotage, not a detail.** Two sabotages first read as
*uncaught* because `-run 'Signal|IsParked'` does not match `TestResolutionUnparksSession`.
A sabotage that appears to survive is a claim about a test that may never have executed —
check the filter before believing it.

### What P4 part 1 does NOT do

- **The kanban card-drawer mount.** Still P4's, unchanged, and `SignalBadge` still stays as
  it is (P5's rule).
- **The structured notification producer.** No tool mints a notification yet; every
  notification row today comes from the turn-end classifier.
- **Routing a worker's blocker to its card** (move to blocked/needs-review). Still gated on
  the user's open sub-question — does a worker on a real blocker **stop** and post, or
  continue past it and post for later review.

Deploy gate unchanged: the server half needs the user's gateway rebuild + restart
(`c251f92c`). Code, build, verify, push — and stop there.
