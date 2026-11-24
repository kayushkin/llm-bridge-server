# kanban-store

A kanban service that stores **where a card sits and what has happened to it**.

Boards, columns and placements live here. Card *content* — title, body, tags,
priority, due date, hold state — lives in a separate [noteboard][noteboard]
service, and kanban-store holds only the noteboard item id. One item can sit on
several boards at once; editing it anywhere changes it everywhere, because there
is only ever one copy.

Its history lives here, though. Every action on a card — created, mail attached,
handed to an agent, note written, moved, held, finished — is one row in an
append-only log, and each row carries what that action meant for the card's
clock. That log is what answers *how long has this taken, how much of it was time
the work could actually be done, and is that inside the limit its priority sets*.
See [Time accounting](#time-accounting).

On top of that it keeps three cross-cutting indexes: **card links**, pointing a
card at an external entity (a repo, a machine, an agent session), **card
assignments**, saying which principals are on a card, and **entity tags**, which
tag those same entities without involving a card at all.

It is deliberately dumb. It does not proxy to the services those entities belong
to, and it does not check that an entity ref exists. It publishes a registry of
entity types so a client can resolve refs itself. The one exception is a card
assignment, which is checked against principal-store before it is written — see
[Card assignments](#card-assignments) for why that one reference is different.

[noteboard]: https://github.com/kayushkin/noteboard

## Why the split

A card on a board and a todo in a list are the same object seen from two angles.
Storing the text in the board makes the board the owner, and then every other
view of that work — a todo list, a search, an agent picking up a task — is
either a copy that drifts or a query that has to know about kanban. Keeping the
text in noteboard and only the *position* here means a card can be created,
completed or held by something that has never heard of a board.

The cost is a hard runtime dependency, described in full under
[The noteboard contract](#the-noteboard-contract).

## Requirements

- Go 1.24+
- A C compiler — the SQLite driver is [`mattn/go-sqlite3`][sqlite3], which is cgo
- A running noteboard (or anything implementing the seven endpoints below)

[sqlite3]: https://github.com/mattn/go-sqlite3

## Running it

```sh
make build
KANBAN_NOTEBOARD_URL=http://localhost:8191 ./bin/kanban-store
```

| Variable | Default | Meaning |
|---|---|---|
| `KANBAN_PORT` | `8305` | Port to listen on; a value that is not a whole number stops the start |
| `KANBAN_DB` | `$HOME/.kanban-store/kanban-store.db` | SQLite file; created with its schema on first run. With no home directory and no value the service refuses to start |
| `KANBAN_NOTEBOARD_URL` | `http://localhost:8191` | Base URL of the noteboard service |
| `PRINCIPAL_STORE_URL` | `http://127.0.0.1:8314` | Base URL of principal-store: the calling principal is read there, and a principal is checked there before a card assignment |
| `LLM_BRIDGE_URL` | `http://127.0.0.1:8160` | Base URL of llm-bridge-server, asked before a board's default agent or instance is written |
| `BUNDLE_STORE_URL` | `http://127.0.0.1:8307` | Base URL of bundle-store, asked before a board's default bundle is written |
| `FILE_STORE_URL` | none | Base URL of file-store. Unset, every attachment route answers 503 |
| `FILE_STORE_SERVICE_TOKEN` | none | Secret. Required when `FILE_STORE_URL` is set |
| `MULTICHAT_URL` | none | Base URL of multichat. Unset, message triggers are recorded `not_configured` and nothing is sent |
| `AUTH_STORE_URL` | `http://127.0.0.1:8303` | Base URL of auth-store, where multichat's API token is resolved |
| `AUTH_STORE_TOKEN` | none | Secret. Required when `MULTICHAT_URL` is set |
| `KANBAN_STORE_SERVICE_TOKEN` | none, required | Secret, at least 32 characters: what internal services present |
| `GRANT_STORE_URL` | none, required | Base URL of grant-store, where board access is read |
| `GRANT_STORE_SERVICE_TOKEN` | none, required | Secret, at least 32 characters: what grant-store asks of this service |

These fourteen are every environment variable the process reads. Each is declared
once, in `internal/config/settings.go`, with llm-bridge's `servicesettings`; the
command reads its configuration from that registry, `GET /settings` describes the
service from it, and a test fails on any `os.Getenv` the declarations do not
name. A set variable that starts with `KANBAN_PORT`, `KANBAN_DB`,
`KANBAN_NOTEBOARD` or `KANBAN_STORE_SERVICE_TOKEN` and is not declared stops the
start and is named in the log: `KANBAN_DB_PATH` is a misspelling, and someone
believes it does something. The service does not own all of `KANBAN_`, because
`KANBAN_STORE_URL` and `KANBAN_URL` are how other services find it.

`systemd/kanban-store.service` is a `--user` unit. `deploy.sh` reads the binary
path, port and database path back *out of* that unit rather than restating them,
builds, runs the test suite, builds the settings registry from the running
service's environment (so a variable that would stop the new binary is found
before the old one is stopped), runs an end-to-end smoke on a throwaway database,
installs, restarts, and rolls back to a timestamped backup if the new binary
will not answer.

⚠️ **`deploy.sh` will not complete a first-ever deploy against an empty
database.** Its final assertion is that `/health` reports more than zero boards,
which exists to catch a binary that silently opened the wrong database file.
On a fresh install that assertion is a false alarm and the deploy rolls back.
Create a board first, or drop that check.

## Security

**Every request except `/health` and `OPTIONS` must carry credentials**: the
service token (`X-Kanban-Store-Service-Token`, internal services) or
`X-Principal-Id`. Anything else is a 401. The rules are in
[Who may see which board](#who-may-see-which-board).

The principal id is trusted as sent. So the store must be reached only through a
gateway that strips any `X-Principal-Id` or service token a client sent and sets
its own from a verified identity; llm-bridge-server's `/kanban/` proxy does. The
server still binds every interface, not loopback (`cmd/kanban-store/main.go`),
and `Access-Control-Allow-Origin` is `*`, so **do not expose the port**: a caller
who can reach it directly can claim to be anyone.

Until 2026-09-18 this section said "there is no authentication and no
authorization". That was true before the access gate and false after it.

## HTTP API

All request and response bodies are JSON.

### Health

| Method | Path | Notes |
|---|---|---|
| `GET` | `/health` | `{"status":"ok","counts":{…}}` — row counts per table |

### Settings

| Method | Path | Notes |
|---|---|---|
| `GET` | `/settings` | What the process was started with: every declared setting, its kind (`wiring`, `path`, `secret`, `behavior`), the value in force and whether it came from the environment or the default. A secret shows only whether it is set. **The service token or an administrator only**: a member is 403 whatever boards they hold, and no caller is 401 |

Nothing is editable, so there is no `PUT /settings/{key}`: a setting changes in
the unit or a drop-in, and takes effect on restart.

### Boards

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/boards` | `?include_archived=true` to include archived boards |
| `POST` | `/api/boards` | `{"name":…,"description":…}`; `name` required |
| `GET` | `/api/boards/{id}` | |
| `PATCH` | `/api/boards/{id}` | Any of `name`, `description`, `archived`, `business_hours`, `default_principal_id`, `default_agent_id`, `default_instance_id`, `default_bundle_id`, `classifier`, `assignment_pool`, `organization_id` — see [Board settings](#board-settings). `taxonomy` is refused here with a 400: see [Board classification](#board-classification) |
| `GET` `PUT` | `/api/boards/{id}/tag-rules` | The board's ordered tag rules — see [Tag rules](#tag-rules) |
| `GET` | `/api/boards/{id}/effective-defaults?tag=…&tag=…` | What a card carrying these tags gets on this board, each default with its source |
| `GET` | `/api/boards/{id}/cards/{card_id}/effective-defaults` | The same, with the card's tags read from noteboard |
| `DELETE` | `/api/boards/{id}` | |

### Board classification routes

See [Board classification](#board-classification) for what these mean.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/boards/{id}/classification` | Taxonomy, `taxonomy_revision`, policy, `supported_modes` and `caller_actions`. Writes nothing. `can_view` |
| `PUT` | `/api/boards/{id}/classification/taxonomy` | `{"taxonomy":{…}}` or `{"taxonomy":null}`, with **`If-Match: "taxonomy-<revision>"`** (428 without, 412 stale). Answers the settings and an affected-work `preview`. `can_administer` |
| `PUT` | `/api/boards/{id}/classification/policy` | `{"axis_policies":{"<axis id>":{"mode":"off\|discovery\|assisted"}}}`, with **`If-Match: "policy-<revision>"`**. `can_administer` |
| `GET` | `/api/boards/{id}/classification/decisions` | Newest first, without source text. `?card_id=`, `?review_state=unreviewed\|reviewed\|needs_review`, `?axis_id=` (and `&value_id=`), `?source_digest=`, `?operation_id=`, `?limit=` (1–100, default 50), `?cursor=`. `total` counts every match. `can_view` |
| `POST` | `/api/boards/{id}/classification/decisions` | A classifier publishes one decision: llm-bridge's `msg.BoardClassificationDecisionPublication`. **Service token only**; 201 new, 200 the same payload again, 409 a different payload under the same key |
| `GET` | `/api/boards/{id}/classification/decisions/{decision_id}` | The decision with its source text and the taxonomy revision it was made under, every review, and the card's labels now. `can_view` |
| `POST` | `/api/boards/{id}/classification/decisions/{decision_id}/reviews` | Accept, correct or reject, by a person (`X-Principal-Id`; the service token is 403). 201 new, 200 a replay. `can_edit` |
| `GET` | `/api/boards/{id}/cards/{card_id}/classification` | The card's labels on this board and their `revision`. `can_view` |

### Columns

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/boards/{id}/columns` | In `position` order |
| `POST` | `/api/boards/{id}/columns` | `{"name":…,"position":…,"color":…,"wip_limit":…,"auto_status":…,"budget_clock_state":…,"lifecycle_state":…,"work_state":…}` |
| `POST` | `/api/boards/{id}/columns/reorder` | `{"columns":[{"id":…,"position":…}]}`; returns the reordered set |
| `GET` `PATCH` `DELETE` | `/api/columns/{id}` | |
| `GET` | `/api/columns/{id}/cards` | One column a page at a time: `?limit=&offset=`, with the column's `total` |

`position` is a float, so inserting between two columns means averaging their
positions rather than renumbering the rest.

`auto_status`, when set to `open`, `done` or `archived`, patches the noteboard
item's status whenever a card is created in or moved into that column. This is
how a "Done" column completes the underlying todo.

`wip_limit` is enforced on create, attach and any move that crosses into the
column — over the limit returns **409**. Moving a card *within* a column it
already occupies is not checked, so a full column can still be reordered.

`work_state` maps the column onto the one vocabulary every board shares —
see [Shared work states](#shared-work-states). Every column carries it, `null`
when nobody has mapped the column; a `PATCH` with `""` clears it.

`budget_clock_state` is what landing in this column means for the card's clock:
`running` (the work is ours to do), `paused` (someone else has the ball) or
`stopped` (it is finished). Unset means a move here says nothing about the clock
and leaves it where it was — the honest default for a column nobody has
classified, and the reason moving between two unclassified columns never invents
a change.

### Cards

A card is a noteboard item plus a placement. Board-scoped operations:

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/boards/{id}/access` | What the caller of this request may do on the board: `{"principal_id":…,"unrestricted":…,"relations":["can_view","can_edit"]}`. `relations` is every relation that holds, weakest first, with the inclusion rule already applied — ask whether the one you need is in it. Needs `can_view`; **404** otherwise |
| `GET` | `/api/boards/{id}/cards` | The assembled board: columns, each with its cards in order, plus `orphans`. Each card carries its shared `work_state` and `work_state_source` — see [Shared work states](#shared-work-states). `?limit=` caps **each column**; `?assignee=`, `?unassigned=`, `?tickets_only=`, `?requester=`, `?channel=`, `?tag=`, `?priority=`, `?status=`, `?due_before=` keep matching cards and `?sort=` orders each column — see [Filtering a board](#filtering-a-board) |
| `GET` | `/api/boards/{id}/card-page` | One global page across columns; required `limit`, optional `offset`. Returns board/column metadata, complete page cards and exact matching `total`. Same board view grants as `/cards` |
| `POST` | `/api/boards/{id}/cards` | Creates the noteboard item **and** places it; `title` and `column_id` required |
| `PUT` | `/api/boards/{id}/cards/{cardID}` | Attaches an *existing* noteboard item; 404 if that item does not exist |
| `DELETE` | `/api/boards/{id}/cards/{cardID}` | Detaches from this board only; the item survives |

Card-scoped operations, across every board the card is on:

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/cards/{id}` | One card read through the gate: `item`, the `placements` the caller can view, `links`, `assignments`, `ticket`, its shared `work_state` with `work_state_source`, and `access` — what the caller may do to it. **404** for an id that is on no board, so this is not a way to read any noteboard item. noteboard checks no caller, so a client that must not see every item reads a card's body here |
| `PATCH` | `/api/cards/{id}` | Forwarded to noteboard unchanged — edit title, body, tags, anything. Send `If-Match: "<the item's updated_at>"` to refuse saving over someone else's change: it goes to noteboard as sent, and noteboard's **412** `{"error":…,"current":{the item as it is now}}` comes back unchanged, with nothing written. No header saves whatever the version |
| `DELETE` | `/api/cards/{id}` | Reversible; `?hard=true` purges the item and drops every placement |
| `POST` | `/api/cards/{id}/move` | `{"board_id":…,"column_id":…,"position":…}` |
| `GET` | `/api/cards/{id}/placements` | Every board this card appears on |
| `POST` | `/api/cards/{id}/hold` | `{"reason":…}` — park the work |
| `POST` | `/api/cards/{id}/unhold` | Release it |

**Hold is the stop/play button, and it lives on the noteboard item, not on the
board.** That is what lets a card be parked in *any* column instead of only by
being dragged into a designated gate, and what makes the pause bind on the
noteboard discovery path — where an agent looks for work and never sees a board
at all. A held card stays visible on its board, since a board is the surface a
human uses to find parked work and resume it.

Held-ness is inherited down noteboard's `parent_id`, and so is the
`auto_hold_at_usd` spend ceiling passed through on create. A sub-card created
without `parent_id` escapes both.

### One command, many cards

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/bulk-card-commands` | The vocabulary: `{"commands":["move","assign","unassign","hold","unhold","add_tags","remove_tags"],"max_cards":200}` |
| `POST` | `/api/bulk-card-commands` | `{"card_ids":[…],"command":…}` plus the one field the command reads: `move` (`{"board_id":…,"column_id":…,"position":…}`), `principal_id` (assign, unassign), `reason` (hold), `tags` (add_tags, remove_tags) |

```json
{"command":"move","succeeded":1,"failed":2,"results":[
  {"card_id":"…","status":200,"response":{"placement":{…}}},
  {"card_id":"…","status":404,"response":{"error":"not found"}},
  {"card_id":"…","status":409,"response":{"error":"column … is at its WIP limit"}}]}
```

**It re-implements no rule.** For each card it builds the request the
single-card route takes and runs it through the same access rules and the same
handler, as the same caller. So a bulk move obeys the WIP limit, applies the
column's `auto_status`, writes its own `card_moved` event naming the caller, and
fires the board's message triggers, card by card; and each `status` and
`response` is that route's own answer, unchanged.

**It is not one transaction.** Each card is its own command, exactly as if sent
alone. A card that is refused undoes nothing done to the others; the answer
says which is which, and the request itself is **200** whenever it was well
formed. A request that is wrong as a whole — no cards, more than `max_cards`, a
card named twice, an unknown command, a field its command does not read — is a
**400** and touches nothing.

`add_tags` and `remove_tags` exist because noteboard's `PATCH` replaces the whole
tag list. They read the card, change only the tags named, and save against the
version they read (`If-Match`), so a card someone else saved in between answers
**412** for that card and keeps their tags. A bulk move puts the first card at
`position` and each one after it one further on, so the cards keep the order
they were sent in.

### Card links

| Method | Path | Notes |
|---|---|---|
| `GET` `POST` | `/api/cards/{id}/time-entries` | Time people log by hand, as `time_logged` events; a correction names the entry it replaces — see [Time people log by hand](#time-people-log-by-hand) |
| `GET` `POST` | `/api/cards/{id}/events` | The action log. `POST {"kind":…,"clock_state":…,"occurred_at":…,"actor":…,"summary":…}` |
| `GET` `POST` | `/api/cards/{id}/notes` | Status updates and summaries written onto the card. Each carries `visibility` — see [Who a note is for](#who-a-note-is-for); `GET ?visibility=requester` keeps only those |
| `GET` | `/api/note-visibilities` | The vocabulary: `["internal","requester"]` |
| `GET` | `/api/cards/{id}/timeline` | `?board_id=` — every action in order, with the time between them and the totals |
| `DELETE` | `/api/notes/{noteID}` | Removes the note's text; its `note_added` event stays |
| `GET` `PUT` | `/api/boards/{id}/priority-levels` | The board's priority ladder |
| `GET` `POST` | `/api/cards/{id}/links` | `{"entity_type":…,"entity_ref":…,"label":…,"occurred_at":…,"clock_state":…}` |
| `DELETE` | `/api/links/{linkID}` | |
| `GET` | `/api/entities/{type}/{ref}/cards` | Reverse lookup: every card linked to this entity, oldest link first, as `{"card_id":…,"item":{…},"work_state":…,"work_state_source":{…}}` |

`entity_type` is not validated against the registry and `entity_ref` is never
resolved. `(card_id, entity_type, entity_ref)` is unique, which makes repeated
linking idempotent.

The reverse lookup returns `card_id`/`item` pairs so a caller can spot
orphans — a `null` item means the noteboard item is gone but the link is not.
The item is noteboard's, unchanged, so it carries the card's `title` and
noteboard `status`; beside it is the card's shared `work_state`. That makes one
call enough for a rollup — project-store reads
`/api/entities/project/project_000001/cards` and counts the open cards by
state. However many cards the entity has, the answer costs one query here (the
links with every placement and column) and one noteboard call
(`POST /api/items/query`); it used to be one noteboard read per card, and one
visibility query per card for a principal.

### Files on a card

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/cards/{id}/attachments` | `[{"card_id":…,"file_id":"file_000001","visibility":…,"attached_by":…,"created_at":…,"file":{…}}]`, oldest first. `?visibility=requester` keeps only those |
| `POST` | `/api/cards/{id}/attachments?filename=…[&visibility=…]` | The body is the file's bytes and `Content-Type` is what they are. **201** and the attachment |
| `GET` | `/api/cards/{id}/attachments/{file_id}/content[?inline=true]` | The bytes, with file-store's headers as it sent them. Range requests work |
| `DELETE` | `/api/cards/{id}/attachments/{file_id}` | **204**. The file comes off the card and is deleted in file-store, reversibly there |

**file-store owns the file; this store owns that it hangs on the card.**
file-store (`127.0.0.1:8317`) keeps the bytes and the record — name, size,
declared and detected type, hash — and `file` on an attachment is that record
passed through unchanged; `null` means file-store no longer has it. This store
keeps which card a file is on, who put it there, and `visibility`, which is a
note's vocabulary and means the same: `internal`, or `requester` as well, and
`internal` when left out.

**A file is reached only through its card.** file-store hands a file to whoever
holds its service token and decides nothing about who may read it, so this store
is the only thing between a person and somebody else's attachment. The routes
sit under `/api/cards/{id}/`, so the gate has held the caller to that card —
`can_view` to list and download, `can_edit` on every board it sits on to attach
and remove — and the download checks that the file hangs on **the card in the
path**: a file id from another card is **404**, however much the caller may see
elsewhere. There is no route that takes a file id alone.

file-store's refusals are the uploader's to read and come through as it wrote
them — **400** for no filename or no content type, **413** over its size limit,
which it serves at `GET /limits`. Its failures are **502**, and leave no row
here. Attaching and removing are `attachment_added` and `attachment_removed`
events on the timeline, carrying the filename, size, type and visibility in
`detail`; neither moves the clock.

**Purging a card destroys its files first** (`DELETE /api/cards/{id}?hard=true`):
a card that is gone can no longer say who may read them. That is every file
file-store holds for the card, **including the ones taken off it earlier**,
which have no row here and which file-store keeps so a removal can be undone. If file-store cannot
destroy them, the answer is **502** and the card is not purged. A reversible
delete of a card leaves its files alone, as it leaves its placements.

Without `FILE_STORE_URL` this store has no attachments and every route above is
**503**: a card's files are then unknown, which is not the same as none. The
token reads every file, so it comes from `~/.config/file-store-tokens.env`
through a host-local drop-in (`kanban-store.service.d/file-store.conf`) — not
the tracked unit, and not the shared principal-gating file, whose variables
every agent session inherits.

⚠️ A browser reaches these routes through a proxy, and a proxy has its own body
limit — nginx's default is 1 MB. An upload refused there never reaches this
store and answers **413** with nginx's page, not file-store's words.

### Time people log by hand

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/cards/{id}/time-entries` | `{"card_id":…,"active_seconds":…,"entries":[…]}`, oldest first. `?active=true` leaves out the replaced ones; `active_seconds` is the card's total either way |
| `POST` | `/api/cards/{id}/time-entries` | `{"seconds":2700,"summary":…,"worked_at":…,"worker_principal_id":…,"board_id":…,"supersedes_event_id":…}` → **201** and the entry |

**A time entry is an event.** It is a `time_logged` event on the card's
timeline, beside the moves and the notes, with `detail`
`{"seconds":…,"worker_principal_id":…,"worked_at":…}`. `worker_principal_id` is
who did the work — a principal-store **human**, checked on write as an assignee
is (400 unknown, disabled, or a group or contact; 502 when principal-store
cannot answer; nothing written). It defaults to the calling principal, and a
caller that is not one must name it. The event's `actor` is who logged it, which
is not always the same person.

**A correction is a new entry that names the one it replaces**
(`supersedes_event_id`). The log stays append-only: the old entry is never
touched, stays on the timeline exactly as it was logged, and is inactive
*because* the new one points at it — `superseded_by_event_id` on the entry and on
its timeline row is computed from that pointer, not stored. Follow the pointers
either way to read an entry's history. Only active entries count, in
`active_seconds` here and in `logged_seconds` on the card's time summary (board
view, column page and timeline). **Withdrawing** an entry is a correction to
`"seconds":0`, which is the one place zero is allowed.

An entry can be replaced **once** — a unique index, so it is a rule of the
database and not of the code that writes. Two corrections of the same entry made
at once cannot both land: one is **201**, the other **409** naming the entry
that won, which is the one to correct instead. A correction takes the board of
the entry it replaces.

**It moves no clock.** Saying how long something took says nothing about whether
the work is runnable, so the event carries the clock state the card was already
in, as an assignment does; logging time on a card that is waiting on its
requester leaves it waiting. And the event happens **when it is logged**: the
clock is the walk of the events in order, and an entry backdated to yesterday
would split an interval already accounted for. When the work was done is
`worked_at`, which may be in the past and not in the future.

`seconds` is at most 31 days, which is there to catch milliseconds sent as
seconds. `POST …/events` refuses `kind: time_logged` and names this route, so
the generic route is not a way around these rules.

### Who a note is for

A note carries `visibility`: `internal`, for the people working the card, or
`requester`, which may also be shown to the person who asked. It is set when the
note is written and never changed. **Left out, it is `internal`** — and so is
every note written before the field existed — because the other default would
show a requester something by omission. A value outside
`GET /api/note-visibilities` is a **400**, on a write and on the `?visibility=`
filter alike: a filter that was dropped would answer the internal notes to a
reader that asked for the requester's.

**This store does not hide internal notes from anyone who can view the card.**
The people working a ticket need those most, and the requester is a
principal-store contact who never calls this store. The field is for a
requester-facing reader — a portal, a reply sent by mail — which asks for
`?visibility=requester` and passes on nothing else.

⚠️ **The timeline carries note text.** A `note_added` event's `summary` is the
note's body, so the event's `detail` is `{"visibility":…}` and a timeline entry
carries the joined note. A requester-facing reader of `/events` or `/timeline`
must drop `note_added` events whose detail is not `requester` — and events
written before 2026-09-18 have no detail, which means `internal`.

### Card assignments

Who is on a card. An assignment is its own fact about the card and not a card
link: a link's `label` is a display name and its uniqueness is per entity ref,
and neither is what "assigned" means. Principals are owned by the
`principal-store` service; kanban-store stores the id it minted
(`principal_000001`) and never the name. There is no role — version one answers
"who is assigned", and owner versus reviewer is a later question.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/cards/{id}/assignments` | `[{"card_id":…,"principal_id":…,"assigned_by":…,"created_at":…}]`, oldest first |
| `PUT` | `/api/cards/{id}/assignments/{principal_id}` | Idempotent: **201** + the row when this call created it, **200** + the unchanged row when it was already there. `assigned_by` is `?actor=` |
| `DELETE` | `/api/cards/{id}/assignments/{principal_id}` | **204**; **404** if the principal was not on the card |
| `GET` | `/api/assignments?principal_id=…` | Reverse lookup: every card this principal is on, across every board, oldest first. **400** without the parameter |

`GET /api/cards/{id}/assignments` returns an `ETag` for the exact assignment snapshot.
Automatic owners can send that value as `If-Match` on the assignment `PUT`.
The snapshot check and insert share a SQLite writer transaction; if any assignment
was added, removed, or replaced, the write returns **412** without adding the owner.
An omitted header retains the existing idempotent assignment behavior. Clients
must not silently retry a 412 with a fresh snapshot: re-evaluate the ownership rule.

Assignments ride along on every card view — `assignments` on each card in
`GET /api/boards/{id}/cards` and `GET /api/columns/{id}/cards` — and the field is
omitted when nobody is on the card.

**This is the one write on which kanban-store checks a reference against the
service that owns it.** On `PUT`, `principal_id` must match `^principal_\d{6,}$`
(**400** otherwise, before anything is called), and then
`GET {PRINCIPAL_STORE_URL}/principals/{principal_id}` is asked, with a 3-second
timeout:

- **404** from principal-store → **400** `principal_id principal_000099 does not exist in principal-store`
- `disabled_at` set → **400** `principal_id … is disabled in principal-store`
- unreachable, timed out, or any other non-200 → **502** `principal-store check failed: …` carrying the transport error verbatim, and **the row is not written**

A link to a session that has gone is a dangling pointer a reader can notice; an
assignment to a principal that does not exist is a silently wrong row forever,
reported by the reverse lookup as work for someone who is not there. Refusing
the write when the check cannot run is the point, not a limitation — the store
never accepts an assignment because it could not ask.

Assigning and unassigning are actions on the card and join its timeline as
`assigned` / `unassigned` events: `summary` is the principal id, `detail` is
`{"principal_id":…}`, and neither carries a board because who is on the card is
true wherever it sits. Only the **201** path of a `PUT` logs one. Neither moves
the clock — see [What each action means for the clock](#what-each-action-means-for-the-clock).

### Entity tags

Tag any `(entity_type, entity_ref)` pair without involving a card — mark a
session `p0`, a machine `lab`.

| Method | Path | Notes |
|---|---|---|
| `GET` `POST` | `/api/entities/{type}/{ref}/tags` | `{"tag":…}` |
| `DELETE` | `/api/entities/{type}/{ref}/tags/{tag}` | |
| `GET` | `/api/tags` | Every tag with its count; `?tag=…&entity_type=…` searches instead |

### Discovery and search

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/entity-types` | The registry: `[{"type":…,"service":…,"search":…,"get":…,"id_patterns":[…],"search_results":…,"label_field":…}]` |
| `GET` | `/api/assignment-strategies` | How a board's `assignment_pool` may choose: `["least_open_cards","round_robin"]` |
| `GET` | `/api/work-states` | The shared work-state vocabulary, each with its meaning: `[{"work_state":"not_started","meaning":…},…]` — see [Shared work states](#shared-work-states) |
| `GET` | `/api/search?q=…` | Full-text, delegated to noteboard; `&limit=`, `&board_id=`, `&on_board=true` |

`/api/entity-types` is how a client discovers where to resolve a ref it finds in
a link or a tag: which service owns that type, and what to append a query to. An
empty `service` means kanban knows the type but no upstream can autocomplete it
— `git_repo` is a remote URL. A `repo` link's ref is a filesystem path; the
`repo` row's `get` and `id_patterns` serve the chat's `repo:<name>` mention
instead, which is the only form its pattern matches. A `commit` is
`<repo>@<sha>` or `<owner>/<repo>@<sha>`, looked up in work-graph-store. An empty `search` on a
type that *has* a service means that service has no search route, so advertising
one would promise a lookup that does not exist. `search_results` and
`label_field` let a client offer a type's records as mentions with no per-type
code: `search_results` is the key of the search answer's object that holds the
array of records (`predictions` for prediction-store's
`{"predictions":[…],"total":n}`), and empty means the answer is itself the
array; `label_field` is the field of each record, in the search answer and in
the `get` answer, that names it for a person (`title`, `display_name`, `name`,
`claim`). Each record's id is its `id` field. Every row with `search`, `get`
and `id_patterns` carries a `label_field`, read off the live service. `session`
has no `search`: llm-bridge-server has no route that filters sessions by `q`
and answers records with an `id`. Edit
`internal/config/entity_types.go` to match your own stack; the list shipped here
is the author's.

`/api/search` passes the query to noteboard and, with `board_id` or
`on_board=true`, keeps only results that are actually placed somewhere.

## Shared work states

**Boards are views, not containers.** One card can sit on several boards, and
each board names its columns its own way — Support Desk's "Waiting on reply",
Northwind's "Blocked". A column's `work_state` maps its name onto one
vocabulary every board shares, so a reader that spans boards — a project
rollup, an orchestrator counting what is waiting on someone — counts cards
without knowing any board's column names.

| `work_state` | Means |
|---|---|
| `not_started` | accepted as work, and nobody has begun it |
| `working` | someone is doing it now |
| `waiting` | blocked on a person or on another piece of work |
| `done` | finished |
| `dropped` | will not be done |

`GET /api/work-states` serves this list; it is the only place the words live.
A value outside it is a **400** on `POST /api/boards/{id}/columns` and
`PATCH /api/columns/{id}`, naming the list, and nothing is written. The word is
taken exactly as written — `Working` is refused, not folded to `working`.

**A card's shared state is the column it last moved into, on any board.** Each
placement's `updated_at` is set when the card is placed on a board and on every
move there (a reorder within a column counts); the placement with the latest
one wins, and a tie goes to the lower board id so every read agrees. The
winner's column decides even when it has no `work_state`: the card then says
`null`, though an older placement's column has one, because the latest move is
the last thing anyone said about the work. The rule is `model.SharedWorkState`.

Cards carry it as `work_state` (`null` when that column has none) and
`work_state_source` — `board_id`, `column_id`, `column_name` and `moved_at` —
on `GET /api/cards/{id}`, on every card of `GET /api/boards/{id}/cards` and
`GET /api/columns/{id}/cards`, on a newly created card, and on each row of
`GET /api/entities/{type}/{ref}/cards`. The state is the card's and every
reader gets it, but **a board the caller cannot view is not named**: its
`board_id`, `column_id` and `column_name` are left out and only `moved_at`
remains. A card on no board has `work_state: null` and no source.

**`work_state` does not touch `auto_status`, and neither implies the other.**
`auto_status` writes noteboard's `open`/`done`/`archived` onto the item when a
card lands in the column; `work_state` writes nothing anywhere and is only
read. Coupling them would make mapping a column silently start editing
noteboard items, and a `dropped` column may or may not want its todos
archived. A column that should also close its todos sets both.

## Time accounting

Three numbers, from one log.

A card's events are walked in order, and **the time between one event and the
next belongs to the state that event put the card into**. Summing the `running`
stretches gives the time the work was actually available to be done; summing
everything gives wall-clock elapsed; the difference is time spent waiting on
somebody else.

The worked example, which lives as a test:

| At | Action | Clock |
|---|---|---|
| 09:00 | mail arrives on a P0 card | running |
| 09:30 | replied, waiting on legal | paused |
| 09:30 **+24h** | legal answers | running |
| +1h | answered the client, done | stopped |

**25.5 hours elapsed. 1.5 hours on the budget clock. A 2-hour limit, met.**
Judging that card on elapsed time alone would have called it thirteen times over
its limit.

Nothing is stored but the events. Every figure is recomputed on read, so there
are no totals to drift out of step with the log, and reclassifying a column
changes what happens next rather than rewriting what already did — each event
keeps the clock state that was true when it happened.

### What each action means for the clock

Everything that puts work in front of us runs the clock (`card_created`,
`card_attached`, `email_received`, `agent_dispatched`, `agent_finished`,
`note_added`, `card_unheld`, `waiting_ended`); everything that hands the ball over
pauses it (`card_held`, `waiting_started`); only finishing stops it
(`card_completed`, `card_detached`). A move takes its state from the destination
column, because that is where a column's classification is for.

`assigned`, `unassigned` and `assignment_skipped` say who is on the card, not whether the work is
runnable, so they carry the clock forward unchanged: each is recorded with the
state the card is already in, and a paused card stays paused and a finished card
stays finished through either. "Already in" means the card's most recent action
on any board; for a card with no history yet — most cards here predate the log —
it is the classification of the column the card sits in, and only a card with no
history in an unclassified column is recorded `running`, the same answer the
store gives any first action on such a card. Like `card_moved`, the two kinds
have no default of their own — posting one by hand to `/events` without a
`clock_state` is a **400**.

`kind` is deliberately open — record an action this service has never heard of and
it is kept. What an unknown kind **cannot** do is guess its own clock state, so
one without an explicit `clock_state` is a **400** rather than a silently invented
budget figure.

Attaching mail or a session is an action and joins the timeline. `email_msgid` is
the same arrival under its RFC identity and would double-count it, `email_sender`
is a learned affinity rather than an event, and a repo or machine link is a fact
about the card — none of the three is logged.

`occurred_at` on a link backdates that action, for the same reason the field
exists on an event: a classifier reads a mailbox on a cadence, so the mail it
files arrived before anything here heard about it. Without it, attaching
week-old mail reported it as arriving now, and a card built by filing a backlog
carried a timeline that began the moment it was filed.

It does **not** move the link's own `created_at`. When the link was recorded and
when the thing happened are two different facts, and events here already keep
them apart as `recorded_at` and `occurred_at`.

`clock_state` overrides what the arrival means for the clock. The link type
already implies one — mail arriving is work landing in front of you, so it runs
the clock — and that is right for a card someone works and wrong for a bucket.
A hundred build digests filed on one No-action card are not a hundred arrivals
of work, and letting them run the clock reports a card nobody has ever touched
as having consumed weeks of budget.

Sending either field with a link type that records no action is a **400**, not a
no-op. Backdating a `repo` link is a caller misunderstanding what the link is,
and answering 201 would let it believe it had moved something on a timeline it
never touched.

### Priority ladders

A board's ladder maps a noteboard priority onto a name and a time limit:

```sh
curl -X PUT localhost:8305/api/boards/$BOARD/priority-levels -d '{"levels":[
  {"priority_value":5,"label":"P0","budget_seconds":7200},
  {"priority_value":4,"label":"P1","budget_seconds":28800},
  {"priority_value":3,"label":"P2","budget_seconds":86400},
  {"priority_value":2,"label":"P3","budget_seconds":604800},
  {"priority_value":1,"label":"P4","budget_seconds":2592000}]}'
```

**P0 is the top rung, and the top rung is the HIGHEST `priority_value`.** noteboard
sorts priority descending and every list view in the stack depends on that, so the
P-number is a label on a rung, not the number in the database.

⚠️ **`priority_value` 0 is reserved and rejected with a 400.** Zero is where
noteboard leaves every card nobody has ranked — the large majority of them — so a
level defined there would promote the entire unranked backlog to most-urgent. An
unranked card gets no rung, no label and no limit.

Each board sets its own ladder, and **a board with no levels ignores priorities
entirely**: its cards carry no limit, whatever their stored priority.

#### Default spend ceiling per rung

A rung may also carry `default_auto_hold_at_usd`: the dollar spend ceiling a card
**starts with** at that priority. It is a default, not a limit the ladder
enforces — kanban-store writes it onto the card's noteboard `auto_hold_at_usd`,
where it stays editable per card, and the scheduler's `spend-ceiling-guard` and
`autoworker` read only the card.

```sh
{"priority_value":5,"label":"P0","budget_seconds":7200,"default_auto_hold_at_usd":20}
```

It is written when:

- a card is created on the board with a priority and no `auto_hold_at_usd`;
- a card with a priority and no ceiling is attached to the board;
- `PATCH /api/cards/{id}` changes `priority` without also sending
  `auto_hold_at_usd`, and the card has no ceiling **or still carries the default
  of the rung it is leaving**. Any other number counts as set by hand and stays.

A rung with no default changes nothing. Editing the ladder does not touch cards
that already exist, and a priority changed in noteboard directly never passes
through kanban-store, so it gets no default. A card on several boards takes the
**lowest** default any of them gives its priority.

### Business hours

A board may declare a working week, and then reports the same elapsed figures a
second way alongside the wall-clock ones — never instead of them:

```sh
curl -X PATCH localhost:8305/api/boards/$BOARD -d '{"business_hours":
  {"tzid":"America/Los_Angeles","days":["MO","TU","WE","TH","FR"],"start":"09:00","end":"17:00"}}'
```

`tzid` is mandatory and never defaulted, for the reason noteboard's recurrence
rules demand one: an offset is not a zone, and hours anchored to one drift an hour
twice a year. A board without hours reports no business figures at all rather than
a guessed nine-to-five. Send `{"business_hours":{}}` to clear them.

**Holidays** are dates on which no business time counts, however the week is
drawn: `"holidays":["2026-11-26","2026-12-25"]` inside `business_hours`. A date
is written `YYYY-MM-DD` and read in the board's own zone, so a holiday starts and
ends at that zone's midnight; anything else, or a date listed twice, is a **400**.
They are stored sorted, at most 500.

⚠️ **Absent and empty differ.** `business_hours` sent *without* `holidays` keeps
the stored ones; `"holidays":[]` clears them. The object is otherwise replaced
whole, and a client that edits only the working week — bridge-ui's settings form
is one — would wipe every holiday on its first save if leaving the field out
meant "none". Clearing the business hours clears the holidays too: they are
dates in its zone.

### Board settings

A board carries the defaults a dispatcher or classifier used to take as flags
on a cron job, so the board is the one place the answer lives and the scheduler
owns only *when* a job runs:

```sh
curl -X PATCH localhost:8305/api/boards/$BOARD -d '{
  "default_principal_id": "principal_000004",
  "default_agent_id":     "17",
  "default_instance_id":  "inst-cc-local",
  "default_bundle_id":    "6",
  "classifier": {"vocabulary":"work","mail_account_ids":["demo-work"],"hold_new_cards":true}}'
```

| Field | Owner asked before the write | Read by |
|---|---|---|
| `default_principal_id` | principal-store (`PRINCIPAL_STORE_URL`) — must exist and not be disabled | kanban-store itself, see below |
| `default_agent_id` | llm-bridge-server (`LLM_BRIDGE_URL`) `GET /agents` — agent-store's **numeric id**, never the slug, which is renameable | the dispatcher that spawns sessions for the board's cards |
| `default_instance_id` | llm-bridge-server `GET /instances/{id}` | the same dispatcher |
| `default_bundle_id` | bundle-store (`BUNDLE_STORE_URL`) `GET /bundles/{id}` — the **numeric id**, not the bundle's name | the dispatcher, which sends it as `bundle_id` on llm-bridge-server's `POST /sessions`; the spawn resolves it through bundle-store and provisions the bundle's tools |
| `classifier.vocabulary` | nobody here — email-classifier owns its vocabularies and refuses a board naming one it lacks (`email-classifier -list-vocabularies`) | email-classifier |
| `classifier.mail_account_ids` | nobody here — mailstack is behind a token this store does not hold; the classifier checks them at run time. Explicit, never "every account" | email-classifier |
| `classifier.hold_new_cards` | — | email-classifier |
| `assignment_pool.principal_id` | principal-store — must be a group and not disabled | kanban-store itself, see below |
| `assignment_pool.strategy` | nobody here — one of `GET /api/assignment-strategies`, else a 400 | kanban-store itself |
| `organization_id` | principal-store — must be a group and not disabled | llm-bridge-server, whose operations classifying the board run under that organization's budget and grants; kanban-store copies it onto every decision. Separate from `classifier.organization_id`, which belongs to mail filing |

An owner that says the id does not exist is a **400** and an owner that could
not be asked is a **502**; nothing is written on either. An empty string clears
an id, `{"classifier":{}}` clears the classifier, `{"assignment_pool":{}}`
clears the pool; a cleared setting is absent
from the board on the wire, not an empty string. Omitting a field leaves it
alone.

**The default principal is applied by kanban-store, not by the caller.** When
a card is created on the board or attached to it and has no assignee, the
board's default goes on it — as an assignment row and an `assigned` event whose
detail says `"source":"board_default"`, with `assigned_by` the creating actor.
A card that already has someone on it is left alone: a default fills a blank
and never overrides a person's choice. The default is re-checked with
principal-store on every application, before the noteboard item is created, so
a person disabled since the setting was made refuses the card with a 400 that
names `default_principal_id` rather than filing new work to someone who has
left.

**An assignment pool hands new work to someone who is working.** With
`"assignment_pool":{"principal_id":"principal_000030","strategy":"least_open_cards"}`,
a card that arrives with no assignee is decided in this order:

1. A tag rule that names a principal wins — it is a per-card override.
2. Otherwise kanban-store asks principal-store which members of the group are
   available now (`GET /principals/{group}/members?available_at=`). Availability
   is each person's own declared week and time off, kept in principal-store;
   this store keeps no hours. One member is chosen by the strategy and the
   `assigned` event's detail says `"source":"pool"`, the strategy, the pool and
   how many `candidates` there were.
   - `least_open_cards` — fewest assignments on this board's cards that sit in
     a column that does not stop the clock (`budget_clock_state` other than
     `stopped`); ties go the round-robin way.
   - `round_robin` — the member whose newest assignment on this board is
     oldest; someone never assigned here first.
   - A tie that remains goes to the lower principal id.
3. Nobody available: the board's `default_principal_id`, with
   `"source":"board_default","pool_empty":true` in the detail.
4. Nobody available and no default: the card arrives unassigned and the log
   gets an `assignment_skipped` event on the board with
   `{"reason":"pool_empty","pool_principal_id":…}`, so the board can show why.

A principal-store that cannot say who is available is a **502** and the card is
not created, the same as for the default principal. Without a pool, nothing
above changes.

### Board classification

A board keeps a classification **taxonomy** — axes, each with values — and a
**policy** saying what happens on each axis. A classifier publishes one
immutable **decision** per card it classified; people add **reviews**; a
card's **labels** on the board are what the reviews set. The shapes a
classifier reads and sends are llm-bridge's (`msg.BoardClassification`,
`msg.BoardClassificationPolicy`, `msg.BoardClassificationDecisionPublication`);
the rest are in `internal/model/classification.go`.

**Ids, not names.** Every axis and value has an id this store hands out
(`classification_axis_000001`, `classification_value_000001`). A PUT sends the
whole taxonomy: an entry with an id keeps it, an entry without one gets a new
one, an id from another board or moved to another axis is a 400, and an id the
current taxonomy has may not be left out — send it with `"archived":true` to
retire it. An archived entry stays so history resolves and can never be chosen
again; a classifier is shown `Selectable()`, which leaves it out. Renaming a
value keeps its id, so every decision on it still means the same thing.

**Revisions.** `taxonomy_revision` counts the board's taxonomies from 1 (0:
never had one) and moves only when the taxonomy does — renaming the board does
not. The same taxonomy sent again writes nothing. Each revision also has
`taxonomy_digest`, llm-bridge's versioned `SemanticDigest` of what a classifier
is shown (the taxonomy's own name is not part of it). Revisions are never
rewritten. A board whose taxonomy was stored by name before revisions existed
was moved once, at start-up, to revision 1 with fresh ids; the table
`classification_taxonomy_migrations` records which name got which id, and a
stored taxonomy that did not parse would have stopped the start.

**Policy.** Per axis id: `off`, `discovery` (proposals recorded and shown,
never reviewed or applied) or `assisted` (a person's review sets the label).
There is no `auto`. An axis the policy does not name is off; a board with no
policy says `"policy": null` and borrows nobody's. The policy is held to the
current taxonomy, an archived axis may only be off, and a taxonomy change that
would break the policy is a 409 until the policy changes first. Changing either
never touches existing decisions or starts any classification.

**Decisions** come only from a classifier with the service token, for an
initiating principal who must hold `can_edit` on the board **when the decision
is published**, not only when the run began. The store refuses one for a
board with no `organization_id`, a card not on the board, a taxonomy revision
or digest it does not have, a policy revision it does not have, an axis that
policy revision has off, a value the taxonomy does not have or has archived,
or a `source_digest` that is not the digest of the `source` sent. The
publication key — operation, card, source digest, taxonomy, policy and prompt
revisions — makes a second publication of the same payload return the first
decision (200) and a different payload under the same key a 409. The source is
kept exactly as sent and returned only by the single decision read; purging a
card removes the source text of every decision about it and keeps the digest.
`model_reported_confidence` is the model's own number for the whole card — not
per axis and not a measured accuracy.

**Reviews and labels.** Only a person's review sets a label: a new decision
never changes one. A review names the axes it looked at (`axis_ids`) — accepting
one says nothing about the others — and sends `expected_labels_revision`, the
card's labels revision it saw; any other is a 409 with the labels as they are
now, and nothing is written. Accept takes the decision's values, correct the
reviewer's, and reject changes nothing. A label another review set can only be
replaced by a review naming that review in `supersedes_review_ids` (409
`supersedes_required` otherwise), so nobody's correction is overwritten by
someone who did not see it. Only an `assisted` axis is reviewed. Reviews are
never changed or removed, and `idempotency_key` — the reviewer's own — makes a
repeated review return the first; the same key with another body is a 409.

Errors on these routes carry `code` beside `error`: a stable word such as
`taxonomy_revision_moved`, `identifier_left_out`, `publication_key_reused`,
`labels_revision_moved` or `supersedes_required`, and `current` where there is
a state to send against. `PATCH /api/boards/{id}` refuses `taxonomy` with a
400 naming the route above.

### Message triggers

A board can send a text message through **multichat** (WhatsApp, Telegram,
Signal, Meta) when something happens to a card on it — a P0 is created, a card
lands in Done, a card is resolved. Triggers are board settings, edited on the
board's settings page; no agent is involved, so a send does not wait for a
permission prompt.

```sh
curl -X POST localhost:8305/api/boards/$BOARD/message-triggers -d '{
  "name":              "P0 created → on-call",
  "event_kind":        "card_created",
  "priority_value":    5,
  "recipient_user_id": "@whatsapp_15551234567:chat.kayushkin.com",
  "recipient_display_name": "On-call",
  "message_template":  "{{.PriorityLabel}} on {{.BoardName}}: {{.Title}} ({{.ColumnName}})"}'
```

| Route | |
|---|---|
| `GET`, `POST /api/boards/{id}/message-triggers` | a board's triggers; create one (enabled) |
| `GET`, `PATCH`, `DELETE /api/message-triggers/{id}` | one trigger; PATCH merges what is sent, `{"enabled":false}` is the off switch, `{"clear_priority":true}` drops the priority filter |
| `GET /api/boards/{id}/message-deliveries?limit=100` | what the triggers did, newest first |
| `GET /api/message-trigger-options` | `event_kinds` a trigger may name, `column_filter_event_kinds` (the ones `to_column_id` applies to), `template_fields` a template may use, and `delivery_configured` |

- **`event_kind`** is one of `card_created`, `card_moved`, `card_completed`,
  `assigned`, `card_held` — served by the options route, never hardcoded in a UI.
  A card-wide event (`card_completed`, `assigned`) is matched against the
  triggers of every board the card is on.
- **`to_column_id`** narrows `card_moved` to moves *into* that column, and must be
  a column of this board; on any other kind it is a 400.
- **`priority_value`** is a rung on this board's ladder — the rung's value, not its
  label, which is renameable. The response carries `priority_label` for display.
  A value that is not a rung is a 400 naming the rungs.
- **`recipient_user_id`** is multichat's id for the person, the bridge puppet id from
  multichat's `GET /api/contacts/unified`. `recipient_display_name` is display only.
- **`message_template`** is a Go `text/template` over `CardID`, `Title`, `BoardName`,
  `ColumnName`, `FromColumnName`, `PriorityLabel`, `EventKind`, `Actor`, `Summary`,
  `AssigneeIDs`, `OccurredAt`. A template that does not parse is a 400; a field it
  names that does not exist fails at delivery and is recorded as such.
- Unknown JSON fields are a 400.

**Delivery.** Recording a card event never waits on a message: the event is
written, the write answers, and the triggers fire in the background. Each
trigger that matches writes one row to `message_deliveries`, unique on
`(trigger_id, event_id)` so an event is delivered at most once per trigger:
`sent` (with multichat's `room_id` and `matrix_event_id`), `failed` (with the
error — multichat refused, auth-store refused, the template named a missing
field, the card could not be read), `pending` (claimed and handed to multichat
with no answer recorded — kanban-store stopped mid-send), or `not_configured`.

**Wiring.** kanban-store calls multichat's `POST /api/messages/send` when
`MULTICHAT_URL` is set (e.g. `http://localhost:8402`), resolving multichat's
API token from **auth-store provider `multichat`** on every send via
`AUTH_STORE_URL` and `AUTH_STORE_TOKEN`; `MULTICHAT_URL` without
`AUTH_STORE_TOKEN` refuses to start. With `MULTICHAT_URL` unset — the state
this service ships in — triggers still match and render, and every delivery is
recorded as `not_configured` with the message it would have sent.

### Tag rules

A board's tag rules override its defaults for cards carrying particular tags —
the card's noteboard tags, matched exactly:

```sh
curl -X PUT localhost:8305/api/boards/$BOARD/tag-rules -d '{"rules":[
  {"tags":["cat:product","urgency:high"], "default_instance_id":"inst-cc-local"},
  {"tags":["cat:product"], "default_bundle_id":"6", "default_agent_id":"12"}]}'
```

- **A rule matches a card that carries every tag it names.** `["cat:product","urgency:high"]`
  does not match a card tagged only `cat:product`.
- **Order decides, per field.** For each of `default_principal_id`, `default_agent_id`,
  `default_instance_id` and `default_bundle_id`, the first matching rule that sets it
  wins; a field it leaves blank falls through to the next matching rule, and past the
  last to the board's own default. Above, a card tagged `cat:product` and `urgency:high`
  runs on `inst-cc-local` (first rule) with bundle `6` and agent `12` (second rule).
- **PUT replaces the whole list, in order.** Send a rule's `id` back to keep it (and its
  `created_at`); omit it to create one. `[]` removes every rule.
- Refused with a **400**: a rule with no tags, a tag with surrounding whitespace, a tag
  named twice, two rules with the same tag set (merge them), a rule setting no default,
  an `id` that is not one of this board's rules, and any unknown JSON key. Every id a
  rule names is checked with its owner exactly as the board's own defaults are — 400
  naming `rules[i]` and its tags when the owner says no, 502 when it cannot answer —
  and nothing is written.

**The resolution lives here and nowhere else.** Ask for it rather than re-implementing it:

```sh
curl "localhost:8305/api/boards/$BOARD/cards/$CARD/effective-defaults"
{"board_id":"…","card_id":"…","tags":["cat:product","urgency:high"],
 "matched_rule_ids":["…","…"],
 "defaults":{
   "default_instance_id":{"value":"inst-cc-local","source":{"kind":"tag_rule","rule_id":"…","rule_tags":["cat:product","urgency:high"],"rule_position":0}},
   "default_bundle_id":{"value":"6","source":{"kind":"tag_rule","rule_id":"…","rule_tags":["cat:product"],"rule_position":1}},
   "default_agent_id":{"value":"12","source":{"kind":"tag_rule","rule_id":"…","rule_tags":["cat:product"],"rule_position":1}}}}
```

A default absent from `defaults` has no value anywhere. `GET
/api/boards/{id}/effective-defaults?tag=…&tag=…` answers the same for a tag list
without reading a card.

**A rule's principal applies on arrival only**, like the board's: when a card is created
on or attached to the board with no assignee, the resolved principal goes on it, and the
`assigned` event's detail says `"source":"tag_rule"` with the `rule_id` and `rule_tags`
(or `"source":"board_default"`). A tag added to a card later assigns nobody.

### Paging a board

Every column carries a `total`, so a client can say *showing 25 of 6,466* rather
than presenting a page as the whole column. `?limit=` on the board view caps each
column; `/api/columns/{id}/cards?limit=&offset=` fetches the rest.

Without a limit the board view still returns everything, so existing callers are
unaffected — but on this host's largest board that is **12 MB and 1.6 seconds per
read**, on a page that polls every fifteen seconds.

**Paging is in stored order unless `?sort=` says otherwise.** What a client
sorts by — priority, due date, title — lives in noteboard, and until 2026-09-20
sorting a whole column meant fetching every item in it, so a client sorted the
page it had. `?sort=` now sends the column's ids to noteboard's
`POST /api/items/query`, which orders the whole column and answers one page of
it — see [Filtering a board](#filtering-a-board). Without `?sort=` a page is
still in stored order and costs nothing extra.

### Filtering a board

`GET /api/boards/{id}/cards` and `GET /api/columns/{id}/cards` take the same
filter. A card must match every parameter sent:

| Parameter | Keeps |
|---|---|
| `assignee=principal_000001` | cards that principal is assigned to |
| `unassigned=true` | cards nobody is assigned to |
| `tickets_only=true` | cards that are tickets |
| `requester=principal_000010` | tickets that contact asked for |
| `channel=email` | tickets that arrived that way, from `GET /api/ticket-channels` |

The filter runs in the query that pages the column, so a page is cut **after**
the filter and each column's `total` counts what the filter keeps — *showing 25
of 310 matching* stays true. A value that cannot mean anything — an id of the
wrong shape, a channel outside the vocabulary, `unassigned=yes`, `assignee`
together with `unassigned=true` — is a **400** naming what would. It is not
dropped: a dropped filter answers every card, which reads as "all of these
match".

A ticket's lifecycle state is not a parameter because it is already a column:
read the columns whose `lifecycle_state` is the one you want.

**By what a card says.** Tags, priority, status and due date live in noteboard,
so those filters — and sorting — are noteboard's to answer:

| Parameter | Keeps or does |
|---|---|
| `tag=billing&tag=urgent` | cards carrying **all** of these tags, matched exactly |
| `priority=3&priority=2` | cards whose priority is **any** of these (a ladder rung's value) |
| `status=open` | cards whose noteboard status is any of these |
| `due_before=2026-09-20T17:00:00-07:00` | cards due before that instant |
| `sort=priority` | the order of each column: one of noteboard's `GET /api/items/query-options` — `given`, `priority`, `due_at`, `updated_at`, `created_at`, `title`. Ties keep the column's stored order |

When any of these is set, a column's card ids — already narrowed by the
parameters above, which run in this store's own SQL — go to noteboard's
`POST /api/items/query`, which filters, sorts and pages them and answers the
page's items in the same call. The page is cut **after both** halves and
`total` counts what matched both. Measured 2026-09-20 on this host's largest
board, 9,506 cards: about 100 ms. When none is set, noteboard is not asked to
filter anything and a read costs what it did before.

This store judges what it can without asking — that a `priority` is a number
and `due_before` an RFC 3339 time — and refuses those itself (**400**). **What a
sort or a status may be is noteboard's vocabulary, and this store keeps no copy
of it**: they go through as written, and noteboard's refusal comes back
unchanged, **400**, in its words. A card whose noteboard item is gone cannot
match such a query, so a filtered or sorted read reports no `orphans`; an
unfiltered one still does.

A page of a column carries each card's `ticket`, as the board view does. It did
not until 2026-09-18, so a ticket list lost its requesters on page two.

## The noteboard contract

kanban-store calls **eight** noteboard endpoints through **nine** client methods — the
table below has nine rows because a plain `DELETE` and `DELETE …?hard=true` are the same
endpoint with a different query. Eight is the number to implement; count
`http.NewRequest` in `internal/noteboard/client.go` to check it. Anything that answers
these can stand in for noteboard — point `KANBAN_NOTEBOARD_URL` at it. Items are treated
as opaque JSON and passed through untouched, so an implementation may carry any
extra fields it likes; the only one kanban-store reads is `id`.

| Called as | Endpoint | Expects | Used for |
|---|---|---|---|
| `CreateItem` | `POST /api/items` | **201**, the created item with an `id` | Creating a card |
| `GetItem` | `GET /api/items/{id}` | **200**, the item; **404** if missing | Assembling a board, verifying an attach |
| `PatchItem` | `PATCH /api/items/{id}` | **200**, the updated item | `PATCH /api/cards/{id}`, and `auto_status` on move |
| `DeleteItem` | `DELETE /api/items/{id}` | **200** | Deleting a card |
| `DeleteItem(hard)` | `DELETE /api/items/{id}?hard=true` | **200** | Purging a card |
| `HoldItem` | `POST /api/items/{id}/hold` | **200**, the item | Parking work |
| `UnholdItem` | `POST /api/items/{id}/unhold` | **200**, the item | Releasing it |
| `Search` | `GET /api/search?q=…&include_held=true&limit=…` | **200**, an array of items | `/api/search` |
| `QueryItems` | `POST /api/items/query` | **200**, `{"total","ids","items","missing_ids"}`; **400** for a filter or sort it refuses | Filtering, sorting and paging a column; the entity reverse lookup's items in one call |

Behaviors it relies on:

- **`POST /api/items` accepts** `type`, `title`, `body`, `tags`, `priority`,
  `list_id`, `due_at`, `parent_id`, `hold`, `hold_reason`, `auto_hold_at_usd`,
  and returns the stored item. Cards are created with `type: "todo"`.
- **`status` takes `open`, `done` or `archived`** — those are the values
  `auto_status` patches, and the only ones a column will accept.
- **A plain `DELETE` is reversible**: it hides the item from reads without
  destroying the row, and does not change its status. kanban-store leaves the
  placements in place on a plain delete precisely because the delete can be
  undone, and drops them only on `?hard=true`. Until a restore, the card still
  holds its slot with a `null` item.
- **`include_held=true` reveals held items in search.** kanban-store always sets
  it: a board must be able to show the work parked on it, or a card could never
  be un-parked.
- **Batch item reads use `POST /api/items/batch {"ids":[…]}`.** The result is
  `{"items":[…]}` in input order, including duplicate ids and null slots for
  missing/deleted records. Full content and held items survive unchanged.
  A failed or malformed batch is an error, never a board full of orphans.
  Deploy noteboard with batch support alongside this client.
  `POST /api/items/query` — which answers the named ids that are live, held
  ones included, in the order sent, and lists the rest in `missing_ids` —
  serves filtered and sorted reads and the entity reverse lookup.

## Development

```sh
make test            # go test ./...
./scripts/e2e-smoke.sh   # boots the real binary against a stub noteboard
./deploy.sh          # vet, test, smoke, install, restart, verify, roll back on failure
```

The end-to-end smoke starts the compiled binary on a throwaway database with a
stub noteboard, then exercises the board lifecycle over real HTTP. It is what
catches the failures `go build` cannot: a Go 1.22+ `ServeMux` route conflict
compiles green and panics at boot.

## License

MIT — see [LICENSE](LICENSE).

## Tickets

A ticket is a card that came from outside. The card is still the card — title,
body and tags in noteboard, placement and history here — and the ticket adds
the facts a card has no room for:

| Field | Meaning |
|---|---|
| `requester_principal_id` | the principal-store **contact** who asked. Checked on write: an unknown or disabled principal is a 400, a principal-store that cannot answer is a 502 and nothing is written, and a `human` or `group` is a 400 telling the caller to resolve the address with principal-store's `POST /contacts/resolve` first |
| `channel` | how it reached us, from `GET /api/ticket-channels` — email, portal, chat, phone, agent. Anything else is a 400 naming the vocabulary |
| `source_entity_type`, `source_entity_ref` | the record that made the card a ticket — for email, `email` and the mailstack locator `<account_id>:<message id>`, the pair the card's email link carries. Optional, and sent together or not at all; the type must be in `GET /api/entity-types`. **Written once**: a write that omits the pair keeps the stored one, the same pair again is accepted, a different pair is a **409**. The card's links show where its mail sits now and merges move them; this shows where the ticket came from. Empty on tickets written before 2026-09-21 that nobody backfilled |

**There is no status field.** A ticket's lifecycle is the column it sits in: a
column carries `lifecycle_state` from `GET /api/ticket-lifecycle-states` (new,
open, waiting_on_requester, resolved, closed), and moving the card is what
changes the ticket's state. A second writable status would disagree with the
column within a week.

A column nobody classified reports **no** state rather than a guessed one —
starting an SLA clock on a guess is worse than reporting nothing — and a card
that sits on two boards answers **both** states, naming each board and column,
because the mail board and the team board can disagree and this store does not
pick between them.

| Route | |
|---|---|
| `GET /api/cards/{id}/ticket` | the ticket, the requester's display name (for rendering only; the id is the handle) and one state per placement. 404 when the card is not a ticket |
| `PUT /api/cards/{id}/ticket` | upsert; an unknown field is a 400, not a silent drop |
| `DELETE /api/cards/{id}/ticket` | the card stops being a ticket and stays exactly where it is |
| `GET /api/tickets` | every ticket the caller can view, newest first, across boards — the conversion log. Each entry is the ticket view above plus `card_created_at`, the card's `card_created` event, so a ticket that arrived as a new card (the two times match) is told apart from an existing card that became one. Absent for cards older than that event (2026-08-21). `?channel=` narrows, `?limit=` (1–200, default 50) and `?before=` (the previous page's `next_before`) page it |
| `GET /api/ticket-channels`, `GET /api/ticket-lifecycle-states` | the vocabularies |

A board view carries each card's ticket, read in one query with the links and
assignments, so a board of thousands of cards is not thousands of reads.
Reading a ticket needs `can_view` on a board the card sits on and writing one
needs `can_edit` on every board it sits on — the same rule as any other card
write.

## Who may see which board

There is no off switch: a store that cannot authorize a caller does not serve
boards. Three settings are required at startup, none with a default:

| Variable | Meaning |
|---|---|
| `KANBAN_STORE_SERVICE_TOKEN` | at least 32 characters; internal services send it as `X-Kanban-Store-Service-Token` and are unrestricted |
| `GRANT_STORE_URL` | where board grants are read, once per request |
| `GRANT_STORE_SERVICE_TOKEN` | sent to grant-store as `X-Grant-Store-Service-Token`; required when grant-store itself enforces principals, or every principal request is a 502 |

Every request except `/health` and `OPTIONS` must carry either the service
token or `X-Principal-Id`; anything else is **401**. **An administrator** —
principal-store's `is_administrator` on a human — is unrestricted, like the
service token: every board, every card, every list, granted or not. It is read
from principal-store on each request, so a demotion bites at once, and a
principal-store that cannot answer is a 502. The principal id is
trusted as sent, so **users must never reach this store except through a
gateway** that removes any `X-Principal-Id` or service token the client sent and
sets its own from a verified login (llm-bridge-server's demo login does this).
grant-store must be given the same token as `KANBAN_STORE_SERVICE_TOKEN`, or it
cannot check that a board exists before granting on it.

Access is per board, from grant-store's `can_view`, `can_edit` and
`can_administer` (each includes the one before it; groups are expanded by
grant-store):

- **Boards**: listing shows only boards the principal can view. Reading a board,
  its columns, cards, ladder, tag rules and defaults needs `can_view`; creating,
  moving, attaching or detaching cards needs `can_edit`; changing the board, its
  columns, ladder or tag rules, and anything about message triggers, needs
  `can_administer`. A principal who creates a board is granted `can_administer`
  on it; if that grant fails the board is deleted again and the answer is 502.
- **Classification**: reading the settings, decisions and labels needs
  `can_view`; reviewing needs `can_edit`; the taxonomy and policy need
  `can_administer`. Publishing a decision takes the service token and no
  principal, an administrator included; the store then checks that the
  decision's initiating principal holds `can_edit` on the board at that moment.
- **Cards**: visible when on at least one board the principal can view;
  changeable only with `can_edit` on **every** board the card sits on, because
  the content is one noteboard item shared by all of them. Attaching an existing
  item needs it to be a card the principal can already see.
- **What may I do?** `GET /api/boards/{id}/access`, and `access` on
  `GET /api/cards/{id}`, answer with the relations that hold for the caller, so
  a client offers only the commands the store would accept. A card's answer
  follows the rule above: a card that also sits on a board the caller cannot
  view is `can_view` only, and that board is not named — not in `placements`,
  and not in the ticket's `states`.
- **Lists** (`/api/search`, `/api/assignments`, `/api/entities/*/cards`, a card's
  placements) are filtered to what the principal can see.
- A board or card the principal cannot view is **404**, the same as one that does
  not exist. One it can view but not change enough is **403**.
- The event log's actor is the principal, whatever `?actor=` says.
- Entity tags and `/api/tags` span every board and are **403** for principals.
- Any route without a rule in `internal/api/principal_access.go` is **403**, so a
  new route stays closed until someone decides who may use it.
- grant-store unreachable is **502**; nothing is served on a guess.

### Global board pages

`GET /api/boards/{id}/card-page?limit=25&offset=0&sort=updated_at` filters and
sorts the entire authorized board before paging. Placement/assignment filters
use this store; the matching ids go to noteboard once, and only the requested
page's complete content comes back. It supports the existing content filters,
plus literal Unicode-insensitive `q`, inclusive RFC3339 `created_from` and
`created_to`, repeated `lifecycle` or `exclude_lifecycle`, exact `card_id`, and
`priority_label` resolved from this board's ladder (mutually exclusive with
numeric `priority`). An unset lifecycle remains eligible unless an inclusion
filter asks for a specific state. `sort` passes to noteboard unchanged, including
`updated_asc`. Ties retain column/placement order. Missing/deleted items are not
matches; the full `/cards` view continues to report them as orphans.

### Conditional board updates

`GET /api/boards/{id}` and successful `PATCH` responses return an `ETag`:
that board's quoted `updated_at` timestamp. Send that exact value in `If-Match`
on a board `PATCH` to reject a stale save with HTTP 412. This endpoint accepts
one quoted board ETag or `*`; malformed/unsupported conditions return 400.
An omitted header preserves the existing API, but a write racing another board
update is rejected with 409. Every update compares the version read in its
SQL UPDATE, so the comparison and write are atomic; even legacy callers cannot
silently replace fields read before another update. A failed save changes nothing.
