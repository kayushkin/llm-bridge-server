# journal-store — API contract

The user's own writing: journal entries, blog posts, notes, and notes about one
person or thing. Kept apart from noteboard, which is mostly agent output and
open to every agent on the host.

It runs inside llm-bridge-server, package
`internal/connectedrecords/journal`, and the server mounts these routes at
`/journal-store/` with the paths below. The records live in Postgres, schema
`journal`, which the module's own role `journal_module` owns; the migrations
are in `migrations/`. All timestamps are epoch seconds (`bigint`, 0 = unset).
Until it moved, it was a service of its own on `127.0.0.1:8322` over SQLite
with no auth; `ImportStandaloneDatabase` copies that database in (below).

## Owners

Every record has an owner: `owner_principal_id`, principal-store's id of the
person who keeps it (`principal_000001`), on every entry, revision, mention
and timeline item. It is set by the store and never sent.

- **Who may call.** The server puts the verified caller on each request. A
  request with none is **401**. A caller that names no principal — the
  server's own service acting as itself — is **403** `journal-store answers
  only the owner's or an administrator's principal` on every route, the
  vocabularies and `/health` included.
- **Reads** reach the caller's own records. An administrator reaches every
  owner's, or one owner's with `?owner_principal_id=`; anyone else may name
  only themselves there (403 otherwise). A record outside reach is **404**,
  exactly as a missing one, and a list holds only records in reach. `/health`
  counts only records in reach.
- **Writes** to a record need it in reach (404 otherwise). What a write
  creates under a record — a revision, a mention, a timeline item — takes the
  record's owner, not the caller's: an administrator who adds an item to
  another person's timeline adds it to that person's timeline.
- **New entries** (`POST /entries`, `POST /imports`) are owned by the caller.
- **Per-owner uniqueness.** `(source, source_ref)` is unique per owner, so an
  import upserts only among the caller's own entries; a published slug is
  unique among one owner's live published entries. Two people may each import
  the same file or publish a post called `hello`. An administrator's
  `GET /published/{slug}` that finds the slug published by two owners is a
  **400** asking for `?owner_principal_id=`.
- **promote-todo** copies a line into noteboard, which keeps no owner and
  which every agent on the host reads, so only an administrator may do it, and
  only on an entry they own. Anyone else is **403** saying why.
- In the database, a child row's foreign key runs over
  `(owner_principal_id, parent)`, so a child can never sit under another
  owner's record.

## `entries`

| Field | Meaning |
|---|---|
| `id` | `entry_000001`. What anything else joins on |
| `kind` | `GET /entry-kinds`: `journal`, `post`, `note`, `about`, `timeline`, `transcript` |
| `title` | required, except for `journal` |
| `body` | markdown |
| `written_at` | the day the entry is about or was written. **Now only when the creating call sent none**; `POST /imports` must send it |
| `visibility` | `GET /visibilities`: `private` (default), `draft`, `published` |
| `published_at` | the **first** time it became published. Going back to draft keeps it |
| `slug` | lowercase letters (any script) and digits joined by single hyphens. **Unique among one owner's live published entries**; drafts and private entries may hold any. Derived from the title when an entry is published with none; editable by `PATCH` |
| `tags` | JSON array of strings, trimmed, deduplicated, never empty strings |
| `about_ref` | `kind` `about`, where it is required, and `kind` `timeline`, where it is optional: the id of what it is about or follows (`person_000012`). Any other kind with one is a 400. `GET /entries?about_ref=` lists them |
| `owner_principal_id` | who keeps it (*Owners*) |
| `source`, `source_ref`, `source_hash` | written by `POST /imports` only. `(source, source_ref)` is unique per owner |
| `created_at`, `updated_at` | |
| `deleted_at` | 0 = live. `DELETE` sets it; `POST /entries/{id}/restore` clears it |
| `mentions` | `GET /entries/{id}` only: the entry's mentions, below |

## Mentions: `[text](ref:<id>)`

A markdown link whose target uses the `ref:` scheme is a mention of `<id>`:
`[Carol](ref:person_000012)`, `[that todo](ref:<noteboard uuid>)`,
`[thread](ref:principal_000003)`. **The id is the truth; the link text is
only what was shown when it was written.** On every create, update, import and
promote-todo the body is parsed and the entry's rows in `mentions` are
rewritten: `entry_id`, `position` (0-based order), `line` (1-based),
`entity_ref`, `entity_type`, `label`. Links with any other target, and links
inside code spans or fenced code blocks, are not mentions.

`entity_type` comes from reference-store's list of reference types, which the
server hands the store from the same process (`NewEntityTypeRegistry`) and
reads again on its own schedule. It is the registry type whose `id_patterns` (anchored,
case-insensitive, as a resolver reads them) **alone** match the ref; when none or
several match it is `''`. A write never waits on reference-store: if the registry
cannot be read, the mention is stored with an empty type, `GET /health` says
`degraded` with the error, and the next good read backfills every empty type it
can classify, across every owner (`BackfillMentionTypes`).

## Routes

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{entries,deleted,published,mentions,revisions,timeline_items}, entity_registry:{types,fetched_at,error}}`, counting only records in reach. `status` is `degraded` when the registry has not been read |
| `GET` | `/entry-kinds` | `["journal","post","note","about","timeline","transcript"]` |
| `GET` | `/visibilities` | `["private","draft","published"]` |
| `GET` | `/entries` | live entries, newest `written_at` first. `?q=` (full-text over title, body, tags; each word a prefix, ANDed; see *Search*), `?kind=`, `?exclude_kind=` (repeatable: leave that kind out), `?tag=`, `?visibility=`, `?about_ref=`, `?source=` (entries `POST /imports` wrote under that source, with their `source_ref`), `?from=` (inclusive) `&to=` (exclusive) on `written_at` in epoch seconds, `?include_deleted=true`, `?limit=` (default 50, max 500), `?offset=`. A bad kind, visibility or number is a 400 |
| `POST` | `/entries` | `EntryInput`: `kind`, `title`, `body`, `written_at`, `visibility`, `slug`, `tags`, `about_ref`. **201**. Any other key is a 400 |
| `GET` | `/entries/{id}` | with `mentions`. A deleted entry is **404** unless `?include_deleted=true` |
| `PATCH` | `/entries/{id}` | any of the `POST` keys. **An unknown key is a 400** naming the accepted ones; `id`, `published_at`, `deleted_at`, `created_at`, `updated_at`, `source*` and `mentions` are 400s saying what moves them; `null` is a 400. A deleted entry is 404 |
| `DELETE` | `/entries/{id}` | soft. **204**; again on a deleted entry is a 204 that changes nothing |
| `POST` | `/entries/{id}/restore` | the entry as it was deleted. A published entry whose slug another has taken since is a **409** |
| `GET` | `/entries/{id}/revisions` | newest first, `?limit=&offset=`; works on a deleted entry |
| `POST` | `/entries/{id}/promote-todo` | `{"line": n}`, 1-based. An administrator on their own entry only. See below. **201** `{todo_id, entry}` |
| `GET` | `/mentions?ref=<id>` | backlinks: `[{entry, mentions}]`, live entries whose body mentions exactly `<id>`, newest written first, each with its mentions of it. No `ref` is a 400 |
| `GET` | `/published` | live published entries, newest `published_at` first; `?tag=`, `?limit=&offset=` |
| `GET` | `/published/{slug}` | one live published entry in reach, or 404 |
| `POST` | `/imports` | bulk upsert, below. `?dry_run=true` answers the same counts and writes nothing |
| `GET` | `/time-precisions` | `["minute","day","month","year"]` |
| `GET` | `/timelines/{id}/items` | the timeline's items, oldest first; `?include_deleted=true`. Not a timeline is a 400 |
| `POST` | `/timelines/{id}/items` | `{"items":[TimelineItemInput…]}`, one transaction: a bad item is a 400 naming its index and writes none. **201** `{items}`. The timeline must be a live entry of kind `timeline` |
| `GET` | `/timeline-items` | **the overlay**: `?timeline=<id>` once per timeline (at least one), `?from=` (inclusive) `&to=` (exclusive) on `occurred_at`, `?limit=` (default 500, max 2000), `?offset=`. Live items of those live timelines, merged oldest first; each carries its `timeline_id`. A timeline out of reach is 404, as a missing one |
| `GET` | `/timeline-items/{id}` | one item; deleted is 404 unless `?include_deleted=true` |
| `PATCH` | `/timeline-items/{id}` | any of `occurred_at`, `ends_at`, `precision`, `title`, `body`, `sources`. Unknown key, `null`, or `id`, `timeline_id`, `deleted_at`, `created_at`, `updated_at` are 400s. Deleted is 404 |
| `DELETE` | `/timeline-items/{id}` | soft, **204** |
| `POST` | `/timeline-items/{id}/restore` | the item as it was deleted |
| `GET` | `/timeline-items/{id}/revisions` | newest first, `[{revision_id, reason, replaced_at, item}]` |

## Timelines

A timeline is an entry of `kind` `timeline`, so it is an `entry_…` id like any
other and shows as a chip. `about_ref` names the person it follows; an event
timeline (a trip, a job) leaves it empty. Its dated things are
`timeline_items`:

| Field | Meaning |
|---|---|
| `id` | `timeline_item_000001` |
| `timeline_id` | the timeline entry |
| `occurred_at`, `ends_at` | epoch seconds; `ends_at` 0 for a moment, otherwise not before `occurred_at` |
| `precision` | `GET /time-precisions`: how much of `occurred_at` to believe. `month` and `year` put it on the first day |
| `title` | required |
| `body` | markdown; `[text](ref:<id>)` links are mentions |
| `sources` | ids the item rests on (a message, a note, an entry), deduplicated |

An item's body links, then its sources (with an empty `label`), are kept in
`timeline_item_mentions`. `GET /mentions?ref=` finds a timeline through a live
item's mention as well as through its own body, and `GET /entries/{id}` on a
timeline lists its body's mentions, then its items', each with `item_id`.
Items are soft-deleted and every update, delete and restore first copies the
whole row to `timeline_item_revisions`. A timeline with live items cannot
change kind (**409**).

## Revisions

Before every update, delete, restore, import update and promote-todo, the whole
row (every column of `entries`) is copied to `entry_revisions` with a
`reason` (`update`, `delete`, `restore`, `import`, `promote-todo`) and
`replaced_at`, in the same transaction as the change: a refused change leaves
no revision. `GET /entries/{id}/revisions` answers `[{revision_id, reason,
replaced_at, entry}]`. Nothing in this store is hard-deleted.

## Imports

`POST /imports {"entries":[ImportEntry…]}`. Each needs `source`,
`source_ref`, `source_hash` and `written_at`; the rest is as `POST /entries`.

- A new `(source, source_ref)` is **created**.
- A known one with the same `source_hash` is **skipped**.
- A known one with another hash is **updated**: `kind`, `title`, `body`,
  `written_at`, `tags`, `about_ref` and `source_hash` are replaced after a
  revision. **`visibility`, `slug` and `published_at` are never touched by a
  re-import** — they are the user's publishing decisions — and a deleted entry
  stays deleted.
- The whole request is one transaction. A bad entry, or one pair twice, is a
  400 and writes nothing.

Answer: `{created, updated, skipped, outcomes:[{source, source_ref, entry_id, outcome}]}`.

## promote-todo

`POST /entries/{id}/promote-todo {"line": n}`, by an administrator on an
entry they own (403 for anyone else, see *Owners*), turns a markdown checklist line
(`- [ ] text`, also `*`, `+`, `1.`, indented, or ticked `[x]`) into a noteboard
todo and rewrites the line to link it: `- [ ] [text](ref:<uuid>)`, keeping the
indent, marker and tick. The todo is `{"type":"todo","title":text,"tags":["journal"],"links":[entry id]}`
with a body naming the entry and line. Line out of range, not a checklist item,
or empty: **400**. Already holds a `ref:` link: **409**. noteboard failing:
**502** and the entry is unchanged. The entry's row is locked
(`SELECT … FOR UPDATE`) from reading the line until it is rewritten, the
noteboard call included, so the line cannot change in between.

## Search

`?q=` on `/entries` searches a generated `tsvector` over title, body and tags
(`simple` configuration, every ASCII punctuation mark read as a space), with a
GIN index. Each whitespace-separated word is a prefix, and the words are
ANDed: `carol` finds `Carol's`, `weath` finds the tag `weather`. A word with
punctuation inside it is a phrase whose last part is a prefix: `well-being`
finds `well-being` and `well being`. Punctuation is never grammar, so a hyphen
or a stray quote cannot make a search fail; a query whose words hold no letter
or digit at all matches nothing. What differs from the SQLite FTS5 search it
replaced: accents are not folded (`cafe` does not find `café`, which FTS5's
default tokenizer matched), and Postgres's parser decides what a word is
outside ASCII punctuation, which can split some non-ASCII punctuation
differently.

## Moving the standalone database in

`ImportStandaloneDatabase(ctx, db, sqlitePath, ownerPrincipalID)` copies a
copy (`sqlite3 .backup`) of the standalone store's file into an empty module,
in one transaction: every entry, revision, mention, timeline item, item
revision and item mention, with every id and time kept and every row owned by
`ownerPrincipalID`. It refuses an empty or malformed owner, a module that
already holds rows, and a file it cannot copy whole (an unknown column, an id
that is not the form its number makes, tags or sources that are not a list of
strings, a row pointing at a record the file lacks). The search column is
built from the copied rows, and each table numbers on after the highest
number copied. `testdata/standalone_schema.sql` is the standalone schema it
expects.

## Reference chips

A resolver probes a reference type's `get` route for an id that matches its
`id_patterns`, and treats 200 as a hit and 404 as an honest miss. The row for
this store is
`{"type":"entry","service":"journal-store","get":"/entries/{id}","id_patterns":["entry_\\d{6,}"]}`.
`GET /entries/{id}` answers 200 with the entry, or 404 for an unknown,
deleted or out-of-reach one, so a chip shows only to someone who may read the
entry.
