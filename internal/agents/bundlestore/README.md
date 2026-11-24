# bundle-store

A small Go service that keeps curated **session bundles** — named, inheritable
sets of **skills and tools**, with optional model and effort defaults — and
composes the right one for a session from the repo's tags and the task's tags,
at `POST /resolve`. It is a module of llm-bridge-server: its records live in
Postgres (schema `bundles`, role `bundles_module`) and its routes are mounted at
`/bundle-store/`, the same paths as the standalone service's behind that prefix.

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. Members are the ids that `skill-store` and `tool-store` assign;
[repo-store](https://github.com/kayushkin/repo-store) supplies a repo's tags;
`llm-bridge-server` calls `/resolve` when it starts a session and applies the
result. `ARCHITECTURE.md` explains how the pieces fit, and the TypeScript types
of the wire format are in `ts/` as `@kayushkin/bundle-store-types`.

## What a bundle is, and why it's its own store

A bundle answers: *"for this kind of repo and this kind of task, which skills and
tools should the session come with?"* Its members are the **ids** `skill-store`
(skills) and `tool-store` (mcp/cli/local tools) assigned, carried with the name
alongside for display. That makes a bundle a
**cross-store join artifact**: it spans skill-store + tool-store (+ model-store for
model/effort), so by the single-source / transparent-layers rules it cannot be owned
by any one of those sibling stores, and it must not live in a harness. Its homes are
therefore either the orchestrator (llm-bridge-server) or — chosen here, because the
definitions are curated, editable, and want a UI/audit trail — its **own store**.

bundle-store owns the **definitions** and the **/resolve join**. It does not own or
duplicate the underlying skills/tools, and it does not provision anything — it returns
`{id, name}` pairs. llm-bridge-server takes the resolved `tools` to tool-store, ensures
the `skills` are installed, and applies `model`/`effort`.

### Members are ids, not names

A member is identified by the id its owning store assigned. This is not a style
preference:

- **Skill names are not unique.** skill-store ingests several upstream
  repositories, and **13 SKILL.md names are carried by two sources each** today
  (`algorithmic-art`, `claude-api`, `skill-creator`, …). A name join picks
  whichever row the query reached first.
- **skill-store cannot be driven by name at all.** Its only install route is
  `POST /skills/{id}/install`. A resolution that hands back names is not
  something a consumer can act on without guessing.

So `bundle_members.member_id` is the identity and `name` is a display label that
is never read back to find a row. Nothing in bundle-store looks a member up by
name: a caller writing a bundle gives each member's id, and a member with a name
and no id is a `400`.

A fresh install holds no bundles; create them through `POST /bundles`. The
owning stores can delete or rename a row without telling bundle-store, so
`CheckMembers` compares every stored bundle with the rows skill-store and
tool-store hold, by member id. An id the owning store no longer carries is a
failure line naming the bundle, kind, id and stored name. An id it still carries
under another name is reported but passes, because the id is what the bundle
means.

## Data model

```
bundles(id, name UNIQUE, display_name, description,
        extends_id FK,      -- parent bundle's id (inheritance), NULL for a root
                            -- ON DELETE RESTRICT; the parent's name is joined
                            -- back on for display and is not stored here
        match_tags JSON,    -- repo/task tags that select this bundle
        denied_read_paths JSON, -- path patterns the session may not read
        model, effort,      -- optional session defaults
        enabled, created_at, updated_at)

bundle_members(bundle_id FK, kind, member_id, name, condition, effect,
        PRIMARY KEY(bundle_id, kind, member_id))  -- kind ∈ {skill, tool}
                            -- member_id: skill-store's id for 'skill',
                            --            tool-store's for 'tool'
                            -- name:      display only, never a lookup key
                            -- effect ∈ {include, deny}; deny only for a tool
```

**Include or deny.** A member's `effect` is `include` (add it to the session;
what a member written without an effect gets, and what every row that existed
before 2026-09-24 became) or `deny` (take the tool away from the session). Deny
exists for tools a harness turns on by default — tool-store's kind `harness`,
such as `claude_code.Write` — and works on any tool. Only a tool can be denied:
a skill deny is a `400`. There is no third state; a tool is added or denied. The
primary key leaves `effect` out, so one bundle either includes a tool or denies
it, never both. `GET /member-effects` serves the list, and the write validates
against that same list.

**Denied read paths.** `denied_read_paths` lists path patterns a session given
the bundle may not read, such as `~/.config/**` or `/proc/**`. Each starts with
`/` or `~/` (anything else is a `400`) and may use `*` and `**`. They are plain
paths, not a rule string for any one harness; the harness wrapper turns them
into its own form.

**Name-keyed members from before `member_id`.** An id cannot be derived from a
name without asking the owning store, and this package never asks. The
standalone store moved such rows into `bundle_members_legacy_name_join` and kept
them; the import copies that table as it is. Nothing resolves those rows. Each
such member has to be re-added through `POST /bundles` with its id.

**Inheritance is an id too.** `extends_id` points at the parent row; the wire
type still carries `extends` as the parent's **name**, read back off the parent
on every load, so a rename passes straight through instead of stranding every
child. A write may supply either one (or both, when they agree) and
`UpsertBundle` settles it once — a parent that does not exist is refused at that
write rather than blowing up later on somebody else's `/resolve`, and so is a
write that would put a bundle in its own inheritance chain. `ON DELETE RESTRICT`
refuses to delete a bundle others extend (`409`, naming them); `SET NULL` would
have turned each child into a root that silently inherits nothing.

`condition` gates a member on a **task tag**: e.g. `chrome-devtools` (a perf/debug
MCP) is only added when the resolve request's `task_tags` include `perf`.

## /resolve — the composition

`POST /resolve {repo_tags, task_tags}` (or `{bundle_ids, task_tags}`) →
`{bundles, skills, tools, denied_tools, denied_read_paths, model, effort}`,
where `bundles` is `[{id, name}]` of bundle-store rows, `skills`/`tools`/`denied_tools`
are `[{id, name}]` of skill-store / tool-store rows, and `denied_read_paths` is
`[string]`. `denied_tools` and `denied_read_paths` are always present, `[]` when
nothing is denied:

1. Select enabled bundles named `base` (universal) or whose `match_tags` intersect
   `repo_tags`.
2. Expand each through its `extends_id` chain, ancestor-first, cycle-guarded (loud
   error on a missing parent or a cycle — no silent truncation, because a truncated
   chain resolves to a smaller bundle that looks perfectly valid).
3. Union members **on the owning store's id**; drop any whose `condition` isn't in
   `task_tags`. Two bundles contributing the same skill collapse to one entry; two
   different skills that merely share a name do not.
4. **A deny anywhere wins.** The resolved set is every selected bundle and every
   ancestor on its extends chain. A tool that any bundle in it denies (with its
   `condition`, if any, in `task_tags`) is left out of `tools` and listed in
   `denied_tools`, even when another bundle in the set includes it, and whatever
   order the bundles were applied in.
5. `denied_read_paths`: the union across the resolved set, in first-seen order,
   without repeats.
6. `model`/`effort`: last non-empty wins across applied bundles.

Pure join — no calls into skill-store/tool-store. Example composition, for a
`base` bundle, a `go-service` bundle matching `go` and a `react` bundle matching
`react` whose `chrome-devtools` member carries the condition `perf`:

```
repo_tags=[go,react,...]            → bundles[base,go-service,react]
                                      skills[backend-patterns,sql-pro,browser-automation,senior-frontend] tools[playwright]
repo_tags=[react] task_tags=[perf]  → tools[chrome-devtools,playwright]   # condition gate fires
repo_tags=[go]                      → skills[backend-patterns,sql-pro] tools[]   # no browser tooling
```

## HTTP API

The paths are relative to `/bundle-store/` on llm-bridge-server, where every
route is an operator route: the rows have no owner, so any caller who could
write one could change every session's bundle.

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | liveness |
| GET | `/bundles?tag=&enabled=true&limit=` | list |
| POST | `/bundles` | upsert (full member set, transactional) |
| GET | `/bundles/{id}` · `/bundles/by-name/{name}` | fetch |
| POST | `/bundles/{id}/enable` · `/disable` | toggle |
| DELETE | `/bundles/{id}` | remove |
| GET | `/member-effects` | the effects a member may carry: `["include","deny"]` |
| POST | `/resolve` | compose a bundle for `{repo_tags, task_tags}` or `{bundle_ids, task_tags}` |

## Decisions taken (in this scaffold)

- **Bundles carry optional `model`/`effort` but NOT `system_prompt`.** Model/effort are
  cheap session defaults; system prompt / identity stays agent-store's domain.
  **Open / to confirm:** precedence when both a bundle and agent-store config set a
  model — current lean is *agent-store wins* (identity beats context), with the bundle
  as the default when agent-store is silent.
- **`/resolve` is a pure id-join.** Validation that referenced skills/tools still
  exist (against skill-store/tool-store) is deliberately out of scope here — add as a
  lint endpoint later rather than coupling resolve to those services.
  `CheckMembers` does that check outside resolve, by id.
- **Curation is hand-first.** Bundles are written through the API; a future
  utility-feedback loop (mining log-store for which members were actually used per
  repo/task) should propose changes that are human-approved, never silently mutate.

## Build, test and import

The package builds as part of llm-bridge-server. Its tests run on throwaway
Postgres databases:

```sh
export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"   # from the repo root
go test ./internal/agents/bundlestore/...
```

`ImportStandaloneDatabase` copies a standalone bundle-store's SQLite file into an
empty module, keeping every id, link, member, time and enabled flag, in one
transaction. It refuses a file its standalone store never finished migrating
(members keyed by name, a parent linked by name, or no `effect` column): start
the standalone store on it once, then import that.

`scripts/e2e-smoke.sh` drove the standalone binary, which is gone; it is kept
for reference until the routes have an end-to-end test of their own.

## License

MIT. See `LICENSE`.
