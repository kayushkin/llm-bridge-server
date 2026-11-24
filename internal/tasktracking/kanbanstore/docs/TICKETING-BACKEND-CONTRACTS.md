# Ticketing backend: contracts before code

Status: proposed, 2026-10-19. Nothing here is built. It fixes the records and
rules for the parts of the Echo ticketing plan that no store on this host owns
yet, so the process boundary can be decided afterwards (see *Open decision*).

The plan this answers is "LLM bridge and email ticketing update plan",
2026-10-19. Its phases 3 and 5 need four things that kanban-store, noteboard,
principal-store, grant-store and llm-bridge-server do not have: a ledger of AI
suggestions, scoped automation rules, support duty, and routing that can hold
work. Mail intake and reply delivery (phase 4) wait on the private mail-store
review and are out of scope here.

## What already exists, and is reused rather than copied

Measured in this repo and its siblings on 2026-10-19.

| Need | Owner today | What it gives |
|---|---|---|
| Ticket content and its version | noteboard | `PATCH` with `If-Match: "<updated_at>"`; a stale write is a 412 carrying the current item, relayed unchanged by `PATCH /api/cards/{id}` |
| Placement, assignment, timing | kanban-store | `card_events` log; assignment `PUT` is idempotent (201 then 200); moves have **no** version check |
| Classification | kanban-store | revisioned taxonomy (`If-Match: "taxonomy-<rev>"`), immutable decisions keyed by publication key, append-only reviews with `idempotency_key` and `expected_labels_revision` |
| Rule-like behavior | kanban-store | tag rules override four board defaults on arrival; message triggers on five event kinds, with `message_deliveries` unique on `(trigger_id, event_id)` |
| Assignment on arrival | kanban-store | tag rule → pool (`least_open_cards` / `round_robin` over principal-store `available_at`) → board default; recorded on the `assigned` event with its source |
| Declared working hours | principal-store | `availability` and `time_off`; "no schedule is unknown, and unknown is never available" |
| Who may do what | grant-store, `is_administrator` | per-resource relations; read at the moment of each effect |
| Agent runs, progress, cancellation | llm-bridge-server | sessions, durable operations with SSE, budgets |
| Sign-in | principal-store identity bindings + llm-bridge-server login sessions | Entra `tid`/`oid` linked to a human by an administrator |

The ticketing backend therefore **never** stores ticket content, placement,
assignees, labels, identities or grants. It stores only its own four records
below, and changes a ticket only by calling the owner with the owner's version
precondition.

## Rules that bind every record below

1. **Effects go through owner commands.** A suggestion accepted, a rule fired
   or a route decided becomes a call to noteboard or kanban-store, never a
   write to a copy. The owner's answer is stored as returned.
2. **Authority is checked when the effect happens, not when it was planned.**
   A rule written by someone who has since lost `can_edit` on the board does
   nothing, and its run records `refused_authority`. A preview is not a grant.
3. **Every effect has an idempotency key**, derived from what caused it
   (`rule_revision_id + card_event_id`, `suggestion_id + accept`), and the
   owner call carries it or is made idempotent by the owner's uniqueness. A
   replay finds the earlier run and does nothing.
4. **Stale is a 412, not a merge.** An effect planned against
   `updated_at = X` sends `If-Match: X`; a 412 records `stale_version` on the
   run with the owner's current item and stops. Nothing retries with a fresh
   version automatically, because the human or rule decided against the old
   content.
5. **Nothing is deleted.** Records move through states; history is rows.
6. **Ids are prefixed** (`suggestion_000001`, `automation_rule_000001`,
   `duty_claim_000001`, `routing_decision_000001`) and registered in this
   repo's entity-type registry, so cards can link to them.

## 1. AI suggestions

A model proposes; a person disposes. Distinct from `echo-agent-notes`, which
publishes an agent's finished report as a note: that is an investigation the
person chose to publish, not a suggestion awaiting acceptance.

**Record `suggestion`**

| Field | Meaning |
|---|---|
| `id` | `suggestion_000001` |
| `card_id`, `board_id` | the ticket and the board whose grants govern it |
| `kind` | served vocabulary: `summary`, `checklist`, `reply_draft`, `labels` |
| `proposal` | JSON, shape per kind; for `labels`, axis and value ids from the board's current taxonomy revision |
| `source_item_updated_at` | the noteboard version the model read |
| `source_taxonomy_revision` | for `labels` |
| `operation_id` | the llm-bridge-server operation that produced it; model, cost and trace live there |
| `state` | `proposed` → `accepted` \| `dismissed` \| `superseded` \| `failed` |
| `decided_by`, `decided_at`, `decision_note` | the person, never an agent principal |
| `effect` | the owner's response to the acceptance call, stored verbatim |

**Invariants**
- Acceptance is a separate request by a human holding `can_edit` on the board
  **at acceptance time**. Accepting applies the proposal with
  `If-Match: source_item_updated_at`; a 412 leaves the suggestion `proposed`
  and returns the owner's current item so the UI can show what changed.
- `labels` acceptance becomes a kanban-store classification **review** by the
  accepting person, with `idempotency_key = suggestion id`. A model's output
  never becomes a review on its own, and never becomes a training example.
- At most one `proposed` suggestion per `(card_id, kind)`. A new one marks
  the old `superseded`. Retrying a failed generation makes a new row; it
  never re-applies an accepted one.

**Routes** (behind llm-bridge-server's login and CSRF)
`GET /cards/{id}/suggestions` · `POST /cards/{id}/suggestions` (starts an
operation; answers `{operation_id, suggestion_id}`) ·
`POST /suggestions/{id}/accept {expected_item_updated_at}` ·
`POST /suggestions/{id}/dismiss {note}`.

## 2. Automation rules

Generalises kanban-store's message triggers to more effects, with audiences
and history. Tag rules stay where they are: they choose defaults on arrival,
which is a different job.

**Record `automation_rule`** — `id`, `audience` (`personal` with a principal
id, `team` with a group id, `organization` with the org group id), `board_id`,
`author_principal_id`, `state` (`draft` \| `active` \| `paused` \| `retired`).

**Record `automation_rule_revision`** — immutable: `rule_id`, `revision`,
`trigger` (a kanban-store event kind from its served list, plus filters on
column, priority, labels), `conditions`, `effects` (ordered list from a served
vocabulary: `add_tags`, `remove_tags`, `move_to_column`, `assign`,
`request_suggestion`, `add_internal_note`), `created_by`, `created_at`.
Activating a revision is its own act.

**Record `automation_run`** — `rule_revision_id`, `card_event_id`, unique on
the pair; `state` `applied` \| `no_match` \| `refused_authority` \|
`stale_version` \| `failed`; per-effect owner responses.

**Invariants**
- Audience is enforced in the backend: a personal rule acts only on cards
  assigned to its owner; a team rule needs the author to be in the group
  **now**; an organization rule needs an administrator to have activated it.
- The author's authority on the board is checked on every run (rule 2).
- **Loops:** a run caused by an event that another automation run wrote is
  recorded and does not fire rules again. kanban-store must mark such events
  (an `actor` of kind `automation` on `card_events`); this is the one change
  this design asks of kanban-store.
- `POST /automation-rules/{id}/preview {revision}` evaluates against the last
  N events and reports what would have happened. It applies nothing and is
  not stored as permission.
- The local tag/participant routing script is retired by porting its rules in
  as `draft` revisions, running both side by side with the script's writes
  switched off, then activating. Its own write path is removed in the same
  change that activates the port.

## 3. Support duty

Being on duty is a claim a person makes, not something inferred. Browser
presence and agent availability are not duty. principal-store's declared
availability says when someone works; duty says they are taking tickets now.

**Record `duty_claim`** — `principal_id`, `queue` (a board id, or a board plus
label filter), `claimed_at`, `last_heartbeat_at`, `ended_at`, `end_reason`
(`released` \| `expired` \| `revoked` \| `disabled`).

**Invariants**
- Expiry is computed from the **current** policy (`heartbeat_ttl_seconds` on
  the queue), never stored on the claim, so a policy change applies at once.
- A claim needs `can_edit` on the board when made, and is ended with
  `revoked` the moment a heartbeat finds it gone.
- One open claim per `(principal_id, queue)`. A heartbeat on an ended claim
  is a 409, and the client must claim again.

## 4. Routing

Replaces assignment-on-arrival only for boards that opt in; boards that do not
keep kanban-store's current behavior unchanged.

**Record `routing_policy`** (revisioned like rules): ordered stages, each
`{candidates: duty_claims | group | principal, strategy: least_open_cards |
round_robin, require: [label conditions]}`, and `when_no_candidate: hold |
leave_unassigned`.

**Record `routing_decision`** — `card_id`, `card_event_id` that caused it,
`policy_revision`, each stage's candidates and why each was dropped, the
chosen principal or `held`, and the kanban-store assignment response.

**Invariants**
- One decision per `(card_event_id, policy_revision)`. Replays find it.
- The assignment is a kanban-store `PUT …/assignments/{pid}`; two routers
  racing on one card both see the assignment already present, and the loser
  records `already_assigned`. kanban-store needs no change for this.
- `held` uses noteboard's hold flag on the card, with the decision id in the
  hold note. Held work is reevaluated when a duty claim starts or a policy
  revision activates, each as a new decision.
- Every decision is explainable from its own row without rerunning it.

## Open decision

**Where these four records live.**
- **(a) A new store, `ticket-store`**, with its own SQLite and the routes
  above, called through llm-bridge-server's gateway like `/kanban/`. Clear
  ownership; one more service to deploy and back up.
- **(b) Tables in kanban-store.** It already holds events, triggers and
  deliveries, so rules 3 and 4 come almost free. But it would then own business
  policy beside placement, which the plan assigns to a separate owner.

Recommendation: **(a)**, because the plan says ticketing owns policy and
kanban-store owns placement, and (b) would make that split hard to undo.
Whichever is chosen, the records and invariants above do not change.

## What kanban-store would need to change

1. An `actor` on `card_events` that can say `automation` with the rule
   revision id (loop prevention, rule 2 above).
2. Optionally a version precondition on moves (`If-Match` on the placement's
   `updated_at`), so a rule's `move_to_column` cannot overwrite a person's
   move made after the rule read the card. Today a move has none.
