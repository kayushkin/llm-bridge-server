# project-store routes

Mounted in llm-bridge-server under `/project-store/`, so `GET /projects` is
`GET /project-store/projects`; every route is an operator route. JSON in and
out unless a route says otherwise. Ids are
`project_000001`, handed out once and never reused. A request body or query
with a field the route does not take is 400. Times are unix seconds.

| Method | Path | What it does |
|---|---|---|
| `GET` | `/health` | `{"status":"ok"}` |
| `GET` | `/vocabulary` | `{"kinds":[…],"stages":[…],"default_stage":…,"entity_types":[{"type","service","meaning"}]}` |
| `GET` | `/projects` | `{"projects":[…]}` by name. Query: `q` (name, goal or id), `kind`, `parent_id`, `include_archived=true` |
| `POST` | `/projects` | `name` (required), `kind` (`project` default, or `stream`), `parent_id`, `goal`, `done_when`, `stage` (`idea` default), `spend_target_share` (0–1 or null), `created_by`. **201** |
| `GET` | `/projects/{id}` | One project |
| `PATCH` | `/projects/{id}` | Any of the create fields but `created_by`, plus `archived` (true/false) and `changed_by`. `parent_id` `""` makes it top level; one that would make the project its own ancestor is 400. Every change is recorded |
| `GET` | `/projects/{id}/changes` | `{"changes":[{"field","old_value","new_value","changed_by","changed_at"}]}`, oldest first |
| `GET` | `/projects/{id}/links` | `{"links":[…]}` what the project owns; `?entity_type=` narrows |
| `POST` | `/projects/{id}/links` | `{"entity_type":…,"entity_ref":…,"note":…,"created_by":…}`. The ref is checked with its own store first: **400** when that store has no such thing, **502** when it cannot answer, **409** when the project already owns it. `label` is that store's name for it, kept for display |
| `DELETE` | `/projects/{id}/links/{link_id}` | **204** |
| `GET` | `/links?entity_type=…&entity_ref=…` | Reverse lookup: which projects own this. `entity_type` is required; without `entity_ref` it lists every link of that type (every filed session, to group sessions by project in one call) |
| `GET` | `/projects/{id}/rollup` | Worked out on request: `project`, `children`, `links`, `cards` (every card kanban-store links to the project: `card_id`, `title`, noteboard `status`, kanban `work_state`), `cards_by_work_state` (open cards only; `unmapped` when the card's column has no state), `repos` (each owned repo's `name` as repo-store has it now, not the link's label, then its `unmerged_branches` and `recent_commits` — the default branch's last 14 days, at most 50 — from work-graph-store, `latest_deploy` from repo-store's ledger and `open_pull_requests` from repo-store), `notes` (every note link's noteboard item: open todos, the project's next steps, first and most urgent first, then the rest newest first), `missing_note_ids` (note links whose item noteboard no longer has) and `suggestions` (proposed, undecided). A store that cannot answer fails the whole call |
| `GET` | `/projects/{id}/activity` | What happened, newest first, worked out on request: `{"since":…,"events":[ActivityEvent…]}`. `?days=` 1–90, default 14. `kind` is one of `/vocabulary`'s `activity_kinds` — field changes, links added (sessions filed, notes and next steps linked), commits on each repo's default branch (first parents, with the session work-graph-store saw make it), deploys, pull requests opened, merged and closed, todos completed, suggestions accepted and dismissed — and says which other fields are set. A store that cannot answer fails the whole call |
| `POST` | `/projects/{id}/next-steps` | `{"title" (required),"body","priority","created_by"}`: makes a noteboard todo tagged `next-step` and links it to the project. **201** with a `LinkedNote` |
| `GET` | `/projects/{id}/suggestions` | `{"suggestions":[…]}` oldest first; `?status=` `proposed`, `accepted`, `dismissed` or `superseded` |
| `POST` | `/projects/{id}/suggestions` | `{"requested_by"}`: asks a model for up to 5 next steps, stores them as `proposed` and supersedes the undecided ones before. **201** `{"suggestions":[…]}`; **502** when no model answers or the answer does not fit |
| `POST` | `/suggestions/{id}/accept` | `{"decided_by","priority"}`: makes the suggestion a next step. **200** `{"suggestion","note"}`; **409** when already decided |
| `POST` | `/suggestions/{id}/dismiss` | `{"decided_by"}`. **200**; **409** when already decided |
| `GET` | `/session-filings` | `{"session_filings":[…]}` newest first: what the session filer concluded about each chat it judged. Query: `status` (`filed`, `proposed`, `accepted`, `dismissed`, `superseded`, `nothing_to_file`), `session_id`, `project_id` (the project picked or filed under), `limit`. `?status=proposed` is what waits for a person |
| `POST` | `/session-filings/run` | `{"since_days","limit"}`, both optional: one filing pass over the chats llm-bridge-server changed in the window (default the last 48 hours; at most 365 days), judging at most `limit` sessions (default 20). **200** with a report: `since`, `chat_sessions`, `filed_by_others`, `already_judged` (by status), `skipped` (by reason), `waiting_for_a_brief`, `judged` (the filings made), `failed` and `remaining`. **502** with the same report when it could not judge some session; **409** while another pass runs |
| `POST` | `/session-filings/{id}/accept` | `{"decided_by","project_id"}`: files the session of a `proposed` filing. `project_id` defaults to the one proposed and is required when none was. **200** `{"session_filing","link"}`; **409** when already decided |
| `POST` | `/session-filings/{id}/dismiss` | `{"decided_by"}`: leaves the session unfiled; the filer does not ask again unless its goal changes. **200**; **409** when already decided |
| `GET` | `/digest` | Every live project's rollup as `text/plain` for an agent: one block each, children indented under their parent |

## What a project owns

`entity_type` is one of `repo` (repo-store's numeric id), `board` (kanban-store
board uuid), `scheduler_job` (scheduler job id), `service` (healthcheck check
name), `principal` (principal-store id), `note` (noteboard item uuid) and
`session` (llm-bridge-server session id: a chat about the project, or a worker
on it, filed because no card or branch ties it there already). One thing may
belong to several projects.

## Session filings

The session filer files chats by itself: a link whose `created_by` is
`project-store session filer` is one it made, and its `note` ends with the id of
the filing (`session_filing_000001`) that holds the evidence. A filing records
`evidence` (`brief`: the chat's brief from llm-bridge-server; `name`: the chat has
no brief, so its name, folder and repos; `none`), the `goal` judged, the
`repo_store_ids` the session moved refs in, the `project_id` the model picked
(`""` when none fits), its `confidence` from 0 to 1, `why`, the `model`, and
`status`. The filer files a pick at or above its minimum confidence (the server's
`project_store.session_filer.minimum_confidence`) as `filed`; anything else is
`proposed` and waits for accept or dismiss. `nothing_to_file` is a chat with no
aim: a test message, or one with no name and no brief. A session someone else
filed is never judged, and filing a session through `POST /projects/{id}/links`
closes its waiting proposal as `superseded`.

A **card** is not linked here. kanban-store holds card links, so a card joins a
project with `POST …/kanban/api/cards/{card_id}/links
{"entity_type":"project","entity_ref":"project_000001"}`, and the rollup reads
them back from kanban-store's reverse lookup.
