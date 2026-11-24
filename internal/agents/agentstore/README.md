# agent-store

Single source of truth for agent identity, nature, and harness configs.

Multiple harnesses — inber, openclaw, dash — each have their own config formats and agent naming. agent-store provides a canonical registry so agents are defined once and configured per-harness. It tracks identity (nature), runtime config (model, tools, limits), file distribution, and drift detection.

```
  ┌─────────────────────────────────────────────────────────┐
  │                    agent-store                          │
  │                                                        │
  │   Agents ──── canonical identity (slug, emoji, role)   │
  │   Nature ──── personality content (identity, values)   │
  │   Configs ─── per-harness runtime settings        │
  │   Tools ───── per-harness tool assignments        │
  │   Status ──── runtime state (idle, working, task)      │
  │   Files ───── distribution tracking + drift detection  │
  │                                                        │
  ╚═══════════╤══════════════════╤═════════════════════════╝
              │                  │
     ┌────────▼────────┐  ┌─────▼──────────┐
     │     inber        │  │   openclaw     │
     │  (harness)  │  │ (harness) │
     └─────────────────┘  └────────────────┘
```

## Where it runs

agent-store is a module of llm-bridge-server. Its records live in Postgres
(schema `agents`, role `agents_module`), built by the embedded migrations in
`migrations/`, and the server mounts its routes. The standalone server, its
settings, its deploy script and the `cmd/seed`, `cmd/test-cycle` and
`cmd/migrate-inber` commands are gone: each opened the SQLite file directly.
`ImportStandaloneDatabase` copies a standalone store's SQLite file into an
empty module in one transaction, keeping every id, and the module numbers on
from that store's own high-water marks.

`New(db, homeDirectory)` is given the folder the scan walks and the prompt
renders into. The store never reads `$HOME`, and it refuses to write, rename
or render a file outside that folder (`ErrWriteOutsideHome`), so a store a
test opens on a temporary folder cannot touch the real home.

```go
store, err := agentstore.New(pool, homeDirectory) // pool from postgresstorage.Open
agentstore.RegisterHandlers(mux, store)
```

Tests run on `postgrestest` databases (`POSTGRESTEST_ADMINISTRATOR_URL`), in
a temporary `$HOME` that `TestMain` sets.

## HTTP API

### Agents

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/agents` | List all agents (add `?expanded=true` for per-harness rows with status) |
| `GET` | `/agents/{slug}` | Get single agent |
| `POST` | `/agents` | Create agent |
| `PUT` | `/agents/{slug}` | Update agent |
| `DELETE` | `/agents/{slug}` | Delete agent (cascades to configs, nature, tools) |

### Orchestrator configs

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/agents/{slug}/harnesses` | Get harness configs for agent |
| `POST` | `/agents/{slug}/harnesses` | Add/update harness config |
| `GET` | `/agents/{slug}/config` | Full runtime config (`?harness=inber`) |
| `GET` | `/configs` | All agent configs for an harness (`?harness=inber`) |

### Operations

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/reconcile` | Check for missing registrations and file drift |
| `GET` | `/health` | Health check, mounted only by `RegisterHealthHandler`, for a host that does not serve its own |

## Schema

### Core tables

| Table | Purpose |
|-------|---------|
| `agents` | Canonical agent definitions — slug, display name, emoji, role |
| `harnesses` | Orchestrator systems (inber, openclaw, dash) with default agent and endpoint |
| `agent_harnesses` | Agent-to-harness configs — model, thinking budget, context tags, limits |
| `agent_harness_tools` | Tool assignments per agent/harness pair |
| `agent_nature` | Identity and personality content (kind: identity, principle, value, user, project) |
| `agent_name_aliases` | Alternative names per context for agent resolution |
| `agent_system_prompt_refs` | Tracks which agents appear in other agents' system prompts |
| `agent_status` | Runtime status (idle, working) with task and session tracking |
| `projects` | Project grouping with slug, name, path |
| `project_nature` | Links projects to nature entries with priority |
| `file_distributions` | Tracks files distributed to harnesses with content hashes |
| `file_scans` | Drift detection — compares current file hash against distributed hash |

### Key concepts

**Agents** are identified by slug (e.g. `claxon`, `brigid`, `fionn`). The same agent can have different configs per harness — different model, different tools, different limits.

**Nature** is the agent's personality content, organized by kind:

| Kind | Description |
|------|-------------|
| `identity` | Who the agent is |
| `principle` | How to operate |
| `value` | What matters |
| `user` | User context |
| `project` | Project-specific knowledge |

**File distribution** tracks when nature content is written out to harness config files (e.g. soul.md). **File scans** detect when those files have been externally modified, enabling a bidirectional sync loop.

## Part of the llm-bridge ecosystem

agent-store is one of several optional stores used by [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server). See the [llm-bridge](https://github.com/kayushkin/llm-bridge) README for the full ecosystem.

## The prompt source

The host prompt lives in this database as **sections**, and a section names no
harness and no file. Every harness gets the same sections; only the way they
arrive differs.

| Table | Holds |
|-------|-------|
| `prompt_collections` | One per place a prompt applies: `global` (rooted at `$HOME`) or `project` (rooted at a directory). |
| `prompt_sections` | Level, title, body, tags, position. Level 1 is a group, level 2 a section in the group above it, level 0 text with no heading. A write sends `level` and `title`; the store composes the markdown heading from them, and the API has no heading field to get wrong. A level-1 section with no body is a group that only holds sections. Files are split at level-1 and level-2 headings, never inside a code fence. |
| `prompt_section_revisions` | Append-only. Every create, update and delete, with its source (`import`, `ui`, `drift`) and a note. Outlives the section. |
| `prompt_collection_outputs` | The files a collection renders to, relative to its root, with `accounted_sha256`: the last disk content the sections are known to account for. |
| `prompt_harness_deliveries` | Per harness id: `inject`, or `native_file` plus the file the harness reads itself. **No default** — an unknown harness is an error. |
| `prompt_drifts` | A rendered file that was edited on disk: the content, the section operations that reproduce it, and what became of it. |
| `tracked_file_ignore_rules` | Copies and third-party files the scan finds but nobody maintains (git worktrees are detected, not pattern-matched). Ignored rows are stamped, never deleted. |

**Render** (`POST /prompt-collections/{id}/render`, and after every section
write) writes the same text to every enabled output. It answers **409** and
writes nothing when any output was edited on disk since the sections last
accounted for it.

**Drift** is how such an edit travels back up. `POST /prompt-drifts/reconcile`
(also run at the end of `POST /files/scan`) diffs the edited file against the
content last accounted for and maps the difference onto sections. A drift that
only edits existing sections is applied at once and the collection re-rendered,
so the collection's other files catch up. One that only adds sections waits
for a tag on each new section and then applies by itself. One that removes a
section or changes a heading is **held** until `POST /prompt-drifts/{id}/apply`;
`…/dismiss` lets the next render overwrite the file instead. Section bodies in a drift are cut from the
file by the splitter — no model writes them. A tagging agent may only annotate
(`PUT /prompt-drifts/{id}/annotation`): tags for added sections, and a note. Applying checks, inside the transaction, that every section of the
edited file is then present in the collection in the file's order.

**Moving a root** (`PUT /prompt-collections/{id}/root`, body
`{"root_path": "/abs/new/root", "slug": "optional-new-slug"}`) is for a repo
whose directory was renamed. In one transaction it sets the collection's
`root_path` (and `slug`, when one is sent; empty keeps it) and renames each
`tracked_files` row at one of the collection's output paths under the old root
to the same relative path under the new root, so the collection keeps its id,
sections, revisions and outputs, and each file keeps its version history. It
touches nothing on disk, and the server log records every move. The scan runs
on its own timer, so it may already have tracked a moved file at its new path;
that row belongs to no collection, so the move merges it into the row that
carries the history (its versions move over, the old row takes its path and
file state) and lists it under `merged_tracked_files`. It answers **400** when
the root is not absolute or the collection is not a `project` collection, and
**409** when another collection has that root or slug, a target path is an
output of another collection, or the collection has an open or held drift.

**Retiring a collection** (`POST /prompt-collections/{id}/retire`, body
`{"note": "why"}`) is for one that should never have existed, such as a
collection an import made for a copy of a repo. No route deletes a collection;
this is the way to take one out of use. In one transaction it deletes every
section, each with a `delete` revision carrying the note, drops the outputs so
nothing renders again, and stamps `retired_at`. Lists leave a retired collection
out, `GET /prompt-collections/{id}` still finds it, `EnsurePromptCollection`
refuses its root, and `ImportUntrackedPromptFiles` leaves files under that root
alone. It touches nothing on disk. It answers **409** for the global collection,
a missing note, one already retired, or one with an open or held drift, and
**404** for an unknown id. The answer lists the deleted section ids and the
removed output paths.

`GET /context/resolve?harness=&work_dir=` returns what the bridge should
inject: the global collection, then each project collection whose root is
`work_dir` or an ancestor. For a `native_file` harness, collections that render
to its file are left out, since the harness reads those itself.

`GET /prompt-delivery-options` serves the vocabularies (delivery kinds, prompt
file names, drift statuses, ignore rule kinds).
