# time-ledger-store contract

time-ledger-store runs inside llm-bridge-server, at `/time-ledger/` with the paths below, and keeps its records in Postgres (schema `time_ledger`, role `time_ledger_module`). It answers only a request carrying the caller the server verified; one with none is 401. JSON everywhere. Times are epoch seconds.

## Owners

Every record has an owner, `owner_principal_id` (principal-store's `principal_000001`), the person whose account of their time it is.

- `POST /time-entries` and `POST /kinds` make a record owned by the caller. A caller that acts as no principal cannot make one (403).
- Revisions, kind assignments, reviews and links take their entry's owner. An entry's parent and its kinds must be the same owner's: a parent or kind of another owner is refused (400) as a missing one is.
- A caller reaches only its own records. A record of another owner answers 404, as a missing one does, on reads and writes alike; lists, `GET /links/{link_type}` and `/health` counts hold only the caller's own. An administrator, or the server's own service, reaches every owner's, and `?owner_principal_id=` narrows any read to one owner. Anyone else may name only itself there (403 otherwise). An administrator's write on an existing entry is made in that entry's owner's account.
- A kind's name is unique within one owner's account, and a live source belongs to one entry **of its owner's**: two people may each keep a `work` kind and an entry from the same calendar date.
- An owner who holds no kind gets the seed kinds (`journey`, `routine`, …) the first time it lists or makes one. Listing every owner's kinds seeds nothing.

## Ids

| Record | Id |
|---|---|
| time entry | `time_entry_000001` |
| kind | `time_entry_kind_000001` |
| review | `time_entry_review_000001` |
| a linked record | the owning store's id, never made here: `calendar_event_…`, `discord_event_…`, event-store's bare number, `chat_message_…`, `trip_…`, `visit_…`, `photo_…`, `transaction_…`, `discord_attendance_…`, `activity_…` |

## Who did it

Every write names `by` (who) and `by_kind`: `program` < `model` < `operator`. A title, span, place, parent, kind or link set by one kind is changed or removed only by the same kind or a higher one (**409** otherwise). A kind or link the operator took off is never put back by a model or a program (**409**). Reviews are the operator's or a model's only.

## Routes

| Method | Path | Body / query | Answer |
|---|---|---|---|
| GET | `/health` | | `{status, counts:{time_entries, kinds, reviews, links}}`, counted in the caller's reach |
| GET | `/vocabularies` | | `{link_types:[{name, table, ref_column, owning_store, can_be_source, has_occurrence, has_overlap, has_amount}], bears_on, actor_kinds, outcomes}` |
| GET | `/kinds` | | `[Kind]` by name |
| POST | `/kinds` | `{name, description, by, by_kind}` | **201** Kind. `name` is lower-case words; taken 409 |
| GET | `/kinds/{id}` | | Kind |
| GET | `/time-entries` | `from`, `to` (entries overlapping `[from, to)`), `ended_before`, `parent` (an id, or `none`), `kind_id`, `outcome`, `q` (substring of title or place, in any case; `%` and `_` match only themselves), `needs_review=true` (no review since the span or a link last changed), `include_deleted=true`, `limit` (default 100, at most 1000), `offset` | `[TimeEntry]` earliest first, without links, children or reviews |
| POST | `/time-entries` | `{title, starts_at, ends_at, all_day, start_date, end_date, time_zone, place, parent_time_entry_id, kind_ids, links:[LinkInput], by, by_kind, reason}` | **201** TimeEntry. One transaction. A source already another entry's is 409 naming it; a child outside its parent 409 |
| GET | `/time-entries/{id}` | | TimeEntry with `links` (removed ones too), `child_ids`, `reviews` |
| PATCH | `/time-entries/{id}` | any of `title, starts_at, ends_at, all_day, start_date, end_date, time_zone, place, parent_time_entry_id` (`""` for none), with `by, by_kind, reason` | TimeEntry. Copies the old one to revisions. A span that leaves a child outside 409; a parent that is the entry or its descendant 409 |
| DELETE | `/time-entries/{id}?by=&by_kind=` | | **204**. Soft: links stay, so its source is not made again. Live children 409 |
| GET | `/time-entries/{id}/revisions` | | `[{revision_id, reason, replaced_at, entry}]` newest first |
| PUT | `/time-entries/{id}/kinds/{kind_id}` | `{by, by_kind, reason}` | TimeEntry |
| DELETE | `/time-entries/{id}/kinds/{kind_id}?by=&by_kind=` | | **204** |
| POST | `/time-entries/{id}/links/{link_type}` | LinkInput with `by, by_kind` | **201** Link, or the identical live one. The same record bearing another way 409 |
| DELETE | `/time-entries/{id}/links/{link_type}?ref=&occurrence_starts_at=&by=&by_kind=` | | **204**. Kept with `removed_at` |
| GET | `/links/{link_type}?ref=&occurrence_starts_at=&include_removed=` | | `[Link]` to that record from every entry in the caller's reach: how an importer finds the entry a source belongs to |
| GET | `/time-entries/{id}/reviews` | | `[Review]` newest first |
| POST | `/time-entries/{id}/reviews` | `{outcome, reason, by, by_kind, model, input_tokens, output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names}` | **201** Review. A model's names its `model` |
| GET | `/reviews/{id}` | | Review |

## Records

- **TimeEntry** `{id, owner_principal_id, title, title_by, title_by_kind, starts_at, ends_at, all_day, start_date, end_date, time_zone, span_by, span_by_kind, place, place_by, place_by_kind, parent_time_entry_id, parent_set_by, parent_set_by_kind, created_by, created_by_kind, created_reason, created_at, updated_at, evidence_changed_at, deleted_at?, outcome, outcome_review_id, kinds:[{kind_id, name, assigned_by, assigned_by_kind, reason, assigned_at}]}`. An all-day entry has `start_date` and `end_date` (exclusive) and runs local midnight to midnight. `outcome` is the newest operator review's, else the newest model review's, else `unknown`. An entry with no end (`ends_at` = `starts_at`) is one whose source gave none.
- **LinkInput** `{link_type, ref, occurrence_starts_at, overlap_seconds, amount_cents, bears_on, reason}`. `bears_on` is `source` (brought the entry in; calendar events, Discord events, event-store events, messages and trips only), `done`, `skipped`, `cost` or `context` (for the reviewer to weigh). `occurrence_starts_at` is required for, and only for, calendar and Discord events: which date of a series, by its original start. `overlap_seconds` is for trips and visits, `amount_cents` for transactions.
- **Link** adds `time_entry_id, linked_by, linked_by_kind, linked_at, removed_at?, removed_by, removed_by_kind`.
- **Review** `{id, owner_principal_id, time_entry_id, outcome, reason, reviewed_by, reviewed_by_kind, model, input_tokens, output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at}`.
- **Kind** `{id, owner_principal_id, name, description, created_by, created_by_kind, created_at}`.

## What changed in the move into llm-bridge-server

- Postgres instead of SQLite; ids, routes and answers are as before, plus `owner_principal_id` on entries, kinds and reviews (and so in the entry a revision keeps). `GET /settings` is gone with the standalone service's own settings. An id that is not the form of its type (`/time-entries/nonsense`) is 404.
- `GET /time-entries/{id}/revisions` and `/reviews` for an entry that does not exist, or is out of reach, are 404; the standalone store answered `[]`.
- `q` matches as `ILIKE` with `%`, `_` and `\` escaped, so a query is a substring and nothing else. The SQLite `LIKE` read `%` and `_` in a query as wildcards, and folded case for ASCII letters only; Postgres folds case for every letter. `offset` below 0 is 400.
- `outcome` filters in SQL before `limit` and `offset`, as the standalone store did in Go; the answer is the same.
- Writes to one owner's account are serialized by an advisory lock, in place of the standalone process's mutex.
- The server's import command copies a standalone store's SQLite file in, through `ImportStandaloneDatabase`: every row keeps its id, every row gets the one owner the import names, and every numbering goes on after the highest number SQLite ever gave (its AUTOINCREMENT counter, which can be past the last row).
- The commands (`time-ledger-import`, `-gather-evidence`, `-review`, `-weekly-summary`, `-attendance-by-kind`) still talk to the store over HTTP; their `-time-ledger-store-url` names where it answers, and they send no credential yet.
