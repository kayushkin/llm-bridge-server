# noteboard

One store for notes, todos and ranked lists. API-first, built to be written to
by programs rather than by a person in a text field.

Every record is an **item**. A `type` of `note`, `todo`, `rank` or `workspace`
says what it is; everything else — markdown body, tags, priority, due date,
recurrence rule, parent — is the same set of columns regardless. That is the
whole design: one table, one API, so a todo can be searched alongside a note and
a program never has to pick which store to talk to.

Nothing here destroys data by accident. `DELETE` is reversible, every write
snapshots the row it replaced, and a purge snapshots before it purges.

## Where it runs

noteboard is a module of llm-bridge-server. Its items live in the Postgres
module `noteboard` (role `noteboard_module`), built by the migrations in
`items/migrations/`, and the server answers its routes under `/noteboard/`
with the same paths behind the prefix: `GET /noteboard/api/items`,
`POST /noteboard/api/items/{id}/hold`, and so on. The server's
`database.postgres_url` (`LLMBRIDGE_POSTGRES_URL`) names the Postgres; with it
empty the routes answer 503 `noteboard_off`.

`llm-bridge-server import-noteboard -sqlite <file>` copies a standalone
noteboard's SQLite file (a copy made with `sqlite3 .backup`) into an empty
module, keeping every item id, revision number, hold, rule and soft-deleted
row. Times keep microseconds, so the nanoseconds the standalone store wrote
are cut off. Text holding U+0000, which Postgres cannot keep, is refused by
name, in the import and on every write.

Packages: `items` is the store, `api` the route handlers, `model` the wire
types and the recurrence rules, `boundedtext` the cap on echoed input.

## Who may call it

Every route is an operator route of llm-bridge-server: the service token, an
administrator, or an administrator's session agent token
(`$LLM_BRIDGE_PRINCIPAL_TOKEN`). The notes are the operator's own and private,
and no row has an owner, so nobody else may read or write them.
`created_by` is a caller-supplied string used as a list filter. **It is not an
identity and nothing checks it.**

## HTTP API

Paths below are behind `/noteboard`. All bodies are JSON. A method a path does not take is the server's plain 405, and an item action no route names its plain 404.

### Items

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/items` | See filters below |
| `POST` | `/api/items` | `type` and `title` required → `201` |
| `GET` | `/api/items/{id}` | `?include_deleted=true` to fetch a tombstone |
| `POST` | `/api/items/batch` | Read `{"ids":[…]}` as `{"items":[…]}` in input order; duplicates retained, null for missing/deleted, held items included, complete content |
| `POST` | `/api/items/query` | Of these ids, which match a filter, in what order — see [Of these items, which match](#of-these-items-which-match). Changes nothing |
| `GET` | `/api/items/query-options` | The sorts and limits that route takes |
| `PATCH` | `/api/items/{id}` | Partial update; see nullable fields below. Conditional with `If-Match` — see [Saving over someone else's change](#saving-over-someone-elses-change) |
| `DELETE` | `/api/items/{id}` | Reversible; `?hard=true` purges |
| `POST` | `/api/items/{id}/restore` | Undo a delete |
| `GET` | `/api/items/{id}/revisions` | Every prior state, newest first; `?limit=&offset=` to page |
| `POST` | `/api/items/{id}/hold` | Optional `{"reason":…}` |
| `POST` | `/api/items/{id}/unhold` | |
| `GET` | `/api/items/{id}/occurrences` | `?from=&to=` RFC3339; `400` if the item has no schedule |
| `POST` | `/api/items/rerank` | `{"items":[{"id":…,"rank":…}]}` |

**Paging the revision history.** A revision carries the whole body it replaced,
so an item that is rewritten often costs the sum of every version of itself to
read back. Measured 2026-08-21 on this box: one item with 308 revisions answered
`GET /revisions` with **246 MB**. `?limit=` and `?offset=` walk it a page at a
time, newest first. The default is still every revision, so nothing written
before paging existed changes. A `limit` or `offset` that is not a non-negative
integer is a `400` rather than a silent 0 — 0 here means unlimited, which is the
opposite of what a caller passing `?limit=abc` was asking for.

**List filters** on `GET /api/items`: `type`, `tag`, `exclude_tag` (repeatable),
`status`, `list_id`, `created_by`, `parent_id`, `include_deleted`,
`include_held`, `limit` (default 100), `offset`, and `sort` — one of `rank`,
`priority`, `updated_at`, `created_at` (default `created_at` descending).

**Nullable fields on `PATCH`** — `tags`, `links`, `due_at`, `schedule` and
`auto_hold_at_usd` track presence, so an explicit `null` clears the field and
leaving it out means "don't touch". Absent and null are different requests.

### Discovery

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/lists` | `list_id` values with their counts |
| `GET` | `/api/tags` | Every unique tag with its count, most-used first |
| `GET` | `/api/search?q=` | Full-text search over title and body; `&type=`, `&tag=`, `&status=`, `&limit=` (default 50), `&include_held=` |
| `GET` | `/health` | `{"status":"ok","items":N}`; **500** if the database cannot be read |

A **list is not a stored row.** `GET /api/lists` derives the set by grouping
items on `list_id`, so a list exists exactly when an item claims it. There is no
`POST /api/lists` — assign `list_id` on an item instead.

## Behaviors worth knowing before you build on it

**Delete is reversible and is not archiving.** `DELETE` stamps `deleted_at` and
leaves the row in place; it drops out of every read path — list, get, search,
tags, lists, count — and `POST /api/items/{id}/restore` brings it back in
exactly the state it left in. It deliberately does **not** touch `status`:
archived is a state you chose for a live item, deletion is the item being taken
away, and collapsing the two means a restore cannot tell them apart. `?hard=true`
purges the row, and snapshots it into revisions before doing so.

**Revisions are partial.** Every update, delete, restore, hold and unhold writes
a snapshot, but the snapshot covers 8 columns — `title`, `body`, `tags`,
`status`, `priority`, `list_id`, `parent_id`, `links`. It does **not** cover
`rank`, `due_at` or `schedule`, and `POST /api/items/rerank` writes `rank` with
no snapshot at all. Overwrite a recurrence rule and the old rule is gone. Read
before you overwrite.

**Held items are hidden by default, and nothing in the response says so.** A
hold is the user parking work behind a gate: the item stays `open` and stays
visible to a human, but `GET /api/items` and `/api/search` withhold it unless
you pass `include_held=true`. Hold is inherited down `parent_id`, so holding a
parent withholds its whole subtree. An unattended worker should leave that
default alone rather than widening its own search with it.

Hold is deliberately not a `status`. Status is the work's lifecycle; hold is who
is allowed to act on it, and an item can legitimately be both open and held.

**`parent_id` is validated on write.** A parent that names no live item, names a
tombstone, or would close a cycle is refused with `400`. Both the hold gate and
the `auto_hold_at_usd` spend ceiling roll up over that edge, so a dangling
parent would let a child escape an inherited hold.

**`auto_hold_at_usd` is a pointer for a reason.** `null` means no ceiling; `0` is
a real ceiling meaning "hold before spending anything". A plain number would make
those two the same request. The dollars actually spent are not stored on the
item.

**`workspace` is its own type**, not a tagged note: an agent's durable working
memory, rewritten every run. It is a type so it can never be mistaken for work
to do, and so the schema can enforce one per job.

### Of these items, which match

```bash
curl -s -X POST "$LLM_BRIDGE_GATEWAY_URL/noteboard/api/items/query" -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN" \
  -H 'Content-Type: application/json' -d '{
  "ids":["<id>","<id>","…"], "tags":["billing"], "priorities":[3],
  "sort":"priority", "limit":25, "offset":0, "include_items":true}'
# → {"total":310,"ids":[…the page, in order…],"items":[…],"missing_ids":[…]}
```

For a caller that owns a set of item ids and none of what the items say.
kanban-store knows which cards sit in a column and nothing of their tags or
priority; filtering or sorting that column used to mean reading every card to
look. It is a `POST` because the ids are the question and a board's worth does
not fit in a URL. It changes nothing.

- An item must match **every** filter set: `tags` (all of them, exactly),
  `priorities` (any), `statuses` (any), `due_before`.
- `sort` is one of `GET /api/items/query-options` — `given` (the default: the
  order the ids were sent in), `priority`, `due_at`, `updated_at`, `created_at`,
  `title`. **Ties fall back to the order sent**, so a page is the same page
  every time and the caller's own order survives inside a tie.
- `limit` and `offset` cut the page **after** the filter and the sort; `total`
  counts the matches. `include_items` answers the page as items too.
- `missing_ids` are the ids sent that name no live item, **whatever the
  filter**: "did not match" and "is gone" are different answers.
- **Held items are answered.** The caller named the ids, exactly as it would to
  `GET` each one; the hold gate is for discovery, which this is not.
- An unknown field, sort, or an empty tag is a **400**: a filter that was
  dropped would answer every item, which reads as "all of these match".

⚠️ **Due dates are compared as instants.** `due_at` is kept as an instant
beside the offset its writer sent, which only the answer uses: `10:00-07:00`
sorts after `12:00Z`, five hours later.

### Saving over someone else's change

Two people open the same item; the first saves; the second's save was made
against a version that is gone. Applied, it overwrites the first one's change
and neither of them sees it happen. A `PATCH` can refuse that:

```bash
# The version is the item's updated_at, exactly as the item carries it.
# GET also sends it as the ETag.
curl -s -X PATCH "$LLM_BRIDGE_GATEWAY_URL/noteboard/api/items/<id>" -H "Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN" \
  -H 'If-Match: "2026-09-18T18:59:29.430695045Z"' \
  -H 'Content-Type: application/json' -d '{"title":"…"}'
```

- The stored `updated_at` is exactly that → the update is applied, **200**, and
  the answer's `ETag` is the new version.
- It is anything else → **412** `{"error":…,"current":{…the item as it is now…}}`.
  Nothing is written and no revision is taken. Show the difference, then retry
  against `current.updated_at`.
- No `If-Match`, or `If-Match: *` → applied whatever the version, as every
  `PATCH` was before this existed. The check is the caller's to ask for.
- An `If-Match` that is not one `updated_at` → **400**. It is not ignored: its
  sender believes the update is conditional.

The check and the write are one step against every other `PATCH`
(`Store.itemUpdateMutex`); without that, of sixteen saves made at once against
one version, as many as fourteen were applied
(`TestOnlyOneOfManyConcurrentSavesAgainstOneVersionWins`). Hold, unhold, delete,
restore and rerank also move `updated_at`, so a save made before one of those is
refused too — the item did change. They take no precondition themselves.

### Recurrence

Any item can carry a `schedule` — an RFC 5545 rule for when it falls due and
when it nudges. `due_at` stays the single source of truth for when the item is
next due; the schedule is the rule that *generates* that answer, and `dtstart`
is the series anchor, not the current due date.

```json
{
  "dtstart": "2026-07-21T09:00:00-07:00",
  "tzid": "America/Los_Angeles",
  "rrule": "FREQ=MONTHLY;BYDAY=3TU",
  "mode": "instance",
  "remind": {"lead": ["-P1D"], "channels": ["digest"]}
}
```

- **`tzid` is mandatory whenever a rule is present and is never defaulted.** An
  offset is not a zone: a rule anchored to `-07:00` cannot know what happens at
  a DST transition and silently drifts an hour twice a year.
- **`mode`** — `instance` (default) materializes each occurrence as a child item,
  so a consumer sees ordinary todos and never has to understand recurrence.
  `rolling` keeps one row whose `due_at` advances on completion, with no history.
- **`remind.lead`** takes ISO-8601 offsets before due (`-P1D`), calendar-aware.
- **`remind.nag`** is a second rule that repeats until the item is done.
- An invalid rule is rejected at write time rather than expanding to nothing.

Check `GET /api/items/{id}/occurrences?from=&to=` before trusting a rule —
`BYDAY=3TU` and `BYDAY=TU;BYSETPOS=3` read alike and diverge in months that
start on a Tuesday. Occurrences are capped at 500 per list, and the response
sets `truncated` when the cap was hit.

`rdate` and `exdate` modify the **due** series only. They do not move the nag: an
extra due date does not buy an extra nag, and a skipped due week still nags.

### Search

`GET /api/search` splits `q` on whitespace into terms, and an item must match
every term. Each term is literal text, never query syntax: a `-`, a quote or
`OR` is punctuation or a word. Punctuation separates words in the text and in
the query, as the FTS5 table this replaced did, so `deploy.sh` is the phrase
"deploy sh", found by `deploy`, `sh` or `deploy.sh`, and a term matches only
when its words stand together in that order. Words are folded to lower case
and never stemmed. A term of punctuation alone is no term. Matches come
newest first. Three things differ from FTS5: accents are kept, so `cafe` does
not find `café`; a word over 2047 bytes is not indexed; and a single item's
search document is limited to the 1 MB Postgres puts on a `tsvector`.

## Development

```sh
export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"   # from the repo root
go test ./internal/tasktracking/noteboard/...
```

## License

MIT — see [LICENSE](LICENSE).

### Query text and creation dates

`POST /api/items/query` also accepts `q` (a literal case-insensitive Unicode
substring across title, body and id), inclusive RFC3339 `created_from` and
`created_to`, and `sort: "updated_asc"`. Filtering occurs before paging and
`total` counts all matches. Percent and underscore characters are literal,
not SQL wildcards. Batch reads have no filters and preserve every requested
slot; an unavailable store fails the batch rather than returning null records.
