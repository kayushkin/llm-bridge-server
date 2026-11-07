# time-ledger-store contract

`127.0.0.1:8343` (`TIME_LEDGER_STORE_ADDR`), no auth: keep it on loopback and put a front door in front. JSON everywhere. Times are epoch seconds.

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
| GET | `/health` | | `{status, counts:{time_entries, kinds, reviews, links}}` |
| GET | `/settings` | | llm-bridge `servicesettings` description |
| GET | `/vocabularies` | | `{link_types:[{name, table, ref_column, owning_store, can_be_source, has_occurrence, has_overlap, has_amount}], bears_on, actor_kinds, outcomes}` |
| GET | `/kinds` | | `[Kind]` by name |
| POST | `/kinds` | `{name, description, by, by_kind}` | **201** Kind. `name` is lower-case words; taken 409 |
| GET | `/kinds/{id}` | | Kind |
| GET | `/time-entries` | `from`, `to` (entries overlapping `[from, to)`), `ended_before`, `parent` (an id, or `none`), `kind_id`, `outcome`, `q` (title or place), `needs_review=true` (no review since the span or a link last changed), `include_deleted=true`, `limit` (default 100, at most 1000), `offset` | `[TimeEntry]` earliest first, without links, children or reviews |
| POST | `/time-entries` | `{title, starts_at, ends_at, all_day, start_date, end_date, time_zone, place, parent_time_entry_id, kind_ids, links:[LinkInput], by, by_kind, reason}` | **201** TimeEntry. One transaction. A source already another entry's is 409 naming it; a child outside its parent 409 |
| GET | `/time-entries/{id}` | | TimeEntry with `links` (removed ones too), `child_ids`, `reviews` |
| PATCH | `/time-entries/{id}` | any of `title, starts_at, ends_at, all_day, start_date, end_date, time_zone, place, parent_time_entry_id` (`""` for none), with `by, by_kind, reason` | TimeEntry. Copies the old one to revisions. A span that leaves a child outside 409; a parent that is the entry or its descendant 409 |
| DELETE | `/time-entries/{id}?by=&by_kind=` | | **204**. Soft: links stay, so its source is not made again. Live children 409 |
| GET | `/time-entries/{id}/revisions` | | `[{revision_id, reason, replaced_at, entry}]` newest first |
| PUT | `/time-entries/{id}/kinds/{kind_id}` | `{by, by_kind, reason}` | TimeEntry |
| DELETE | `/time-entries/{id}/kinds/{kind_id}?by=&by_kind=` | | **204** |
| POST | `/time-entries/{id}/links/{link_type}` | LinkInput with `by, by_kind` | **201** Link, or the identical live one. The same record bearing another way 409 |
| DELETE | `/time-entries/{id}/links/{link_type}?ref=&occurrence_starts_at=&by=&by_kind=` | | **204**. Kept with `removed_at` |
| GET | `/links/{link_type}?ref=&occurrence_starts_at=&include_removed=` | | `[Link]` to that record from every entry: how an importer finds the entry a source belongs to |
| GET | `/time-entries/{id}/reviews` | | `[Review]` newest first |
| POST | `/time-entries/{id}/reviews` | `{outcome, reason, by, by_kind, model, input_tokens, output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names}` | **201** Review. A model's names its `model` |
| GET | `/reviews/{id}` | | Review |

## Records

- **TimeEntry** `{id, title, title_by, title_by_kind, starts_at, ends_at, all_day, start_date, end_date, time_zone, span_by, span_by_kind, place, place_by, place_by_kind, parent_time_entry_id, parent_set_by, parent_set_by_kind, created_by, created_by_kind, created_reason, created_at, updated_at, evidence_changed_at, deleted_at?, outcome, outcome_review_id, kinds:[{kind_id, name, assigned_by, assigned_by_kind, reason, assigned_at}]}`. An all-day entry has `start_date` and `end_date` (exclusive) and runs local midnight to midnight. `outcome` is the newest operator review's, else the newest model review's, else `unknown`. An entry with no end (`ends_at` = `starts_at`) is one whose source gave none.
- **LinkInput** `{link_type, ref, occurrence_starts_at, overlap_seconds, amount_cents, bears_on, reason}`. `bears_on` is `source` (brought the entry in; calendar events, Discord events, event-store events, messages and trips only), `done`, `skipped`, `cost` or `context` (for the reviewer to weigh). `occurrence_starts_at` is required for, and only for, calendar and Discord events: which date of a series, by its original start. `overlap_seconds` is for trips and visits, `amount_cents` for transactions.
- **Link** adds `time_entry_id, linked_by, linked_by_kind, linked_at, removed_at?, removed_by, removed_by_kind`.
- **Review** `{id, time_entry_id, outcome, reason, reviewed_by, reviewed_by_kind, model, input_tokens, output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at}`.
