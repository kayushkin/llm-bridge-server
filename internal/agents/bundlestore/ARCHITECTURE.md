# Session Bundling — Architecture

How a session automatically comes up with the right skills + tools + MCPs for the repo
it's working in and the task it's doing. This spans three components; `bundle-store` and
`repo-store` are the two new ones, and `llm-bridge-server` orchestrates.

## Principle

Individual harnesses (jig, claudecode, codex, …) must **not** own this — it's an
abstractable, cross-harness capability. A bundle is also a **cross-store join**
(skills + tools + model), so it can't be folded into any sibling single-domain store
(tool-store / skill-store / agent-store) without that store reaching across domains it
doesn't own. Hence: dedicated stores for the curated state, and the orchestrator for
the stateless join + application.

## Responsibility decomposition

| # | Responsibility | Nature | Home |
|---|---|---|---|
| 1 | Repo signature (repo → tags) | pure computation + cached metadata | **repo-store** (wraps the `repodetect` logic in `detect.go`) |
| 2 | Task signature (prompt → tags) | synchronous model inference | a resolver capability in llm-bridge-server (not a service) |
| 3 | Bundle definitions (curated base/overlays) | persistent curated cross-domain state | **bundle-store** |
| 4 | Resolution (tags → concrete {skills,tools,model}) | stateless join | **bundle-store** `/resolve` |
| 5 | Application (bundle → harness config) | exists today | **llm-bridge-server** `tool_provision.go` |
| 6 | Utility priors (usage → weights) | batch job + stored weights | scheduler job → bundle-store (future) |

## The three boxes

```
 ┌───────────┐   repoPath → {tags}          ┌─────────────┐   {repo_tags, task_tags}
 │ repo-store│ ───────────────────────────► │ bundle-store│ ◄────────────────────────
 │  :8306    │   detect.go (repodetect lib) │   :8307     │   /resolve (pure join over
 └───────────┘                              └─────────────┘    curated defs + inheritance)
       ▲                                           │ → {skills[], tools[], model, effort}
       │ (also importable as a library)            ▼
 ┌──────────────────────────────────────────────────────────────────────────────────┐
 │ llm-bridge-server  (orchestrator — bootstrap, between session-create and spawn)    │
 │  1. repoTags  = repo-store /detect or /by-path  (the target repo)                  │
 │  2. taskTags  = classify(prompt)  [Haiku, like kanban-classifier]                  │
 │  3. resolved  = bundle-store /resolve(repoTags, taskTags)                          │
 │  4. provision = tool-store /provision(resolved.tools)  → mcpServers → --mcp-config │
 │  5. ensure resolved.skills installed (skill-store); apply resolved.model/effort    │
 └──────────────────────────────────────────────────────────────────────────────────┘
```

### Insertion point (verified)

llm-bridge-server already does step 4's shape. `internal/server/tool_provision.go`
reads a `tool_store_tools` array out of a session's `HarnessConfig`, calls tool-store
`/provision`, writes the returned MCP config to a tmpfile, and rewrites `HarnessConfig`
with `mcp_config` — all **after** `handleCreateSession` (`sessions.go:~132`) and
**before** the harness spawns in `startOnInstance`. The resolver hooks in right there,
alongside the existing hook chain (`hook_settings.go`): it fills `tool_store_tools`
(and a skills list) from the resolved bundle instead of requiring the caller to specify
them. The server↔resolver contract — `resolve(repoTags, taskTags) → {skills, tools,
model, effort}` — is identical whether resolution runs in-process or via bundle-store,
so an in-process first cut can be extracted to bundle-store with no flow change.

## repodetect + the inber migration

Detection logic lives in `repo-store/detect.go` as a pure `Detect(path) → Signature`
function (the single source of the ruleset), served by repo-store and importable as a
library. It **replaces** the only existing repo classifier:
`inber/engine/workflow_hooks.go:44-58` `detectProject()` — a private, **first-match**
(`go.mod` → `"go"`, return) detector used to pick build/format/test commands
(`workflow_build.go:46`, `workflow_format.go:15`, `workflow_deploy.go:81`). Its
first-match behavior is wrong for full-stack repos (dash has both `go.mod` and
`package.json`); repodetect collects all signals. inber becomes a consumer.

## Skills flow ≠ tools flow

Tools are provisioned through `HarnessConfig` → tool-store `/provision` →
`--mcp-config`. **Skills** are discovered by Claude Code from `~/.claude/skills`
(installed out-of-band by skill-store), so a resolved bundle's `skills` means
*ensure-installed + surface*, not provision. Note skill-store's `tags` column exists but
its search is `LIKE` on name/description only (`skill-store/store.go` `ListSkills`);
exposing tag filtering there is a small, useful adjacent change. A skill installed only under
`~/.claude/skills` is invisible to the registry until skill-store ingests it, so ingest
any skill a bundle names before relying on registry discovery.

## Tag taxonomy is the unifying primitive

repo-store tags, tool-store tags (`?tag=`), and bundle `match_tags` should share one
vocabulary (`react`, `web`, `frontend`, `go`, `perf`, …). Then bundle assembly is
"union by tag" rather than hardcoded lists. tool-store already tag-filters; repo-store
emits tags; skill-store needs tag filtering exposed.

## Decisions taken

- Two new **stores** (repo-store, bundle-store); **zero** new services for detection
  (library + repo-store) or task classification (resolver capability) or utility
  (scheduler job). forge is **untouched** (it owns ephemeral worktrees, not canonical
  repo identity).
- bundles carry optional `model`/`effort`, not `system_prompt` (agent-store's domain).
  Open: model precedence vs agent-store (lean: agent-store wins, bundle is the default).
- `/resolve` is a pure **id**-join; existence validation against skill/tool stores is a
  later lint, not a coupling. A member is the id its owning store assigned, with the
  name kept alongside for display — skill-store carries 13 SKILL.md names from two
  sources each, and installs only by `POST /skills/{id}/install`, so a name is neither
  unique nor actionable. Nothing in bundle-store turns a name into an id: a writer gives
  the id. `bundle-store -check-members` reads the stored bundles and confirms each member
  id still exists in its owning store; `deploy.sh` runs it before stopping the service.

## Phasing

1. **Now (this scaffold):** repo-store + bundle-store stand alone and resolve correctly.
2. **Phase 1:** in-process resolver in llm-bridge-server (repo-store tags → bundle-store
   /resolve → existing provision). Adopt `repodetect` in inber. Expose skill-store tag
   filtering.
3. **Phase 2:** task classifier (Haiku) feeding `task_tags`.
4. **Phase 3:** utility-feedback loop — scheduler job mines log-store for member usage
   per (repo-tag × task-tag), proposes curated bundle edits, human-approved.

## Status

Both stores are built, tested and running as systemd user services. llm-bridge-server
calls `/resolve` with `bundle_ids` when it starts a session with a chosen bundle, and
applies the result. Selection by repo and task tags at session start is not wired yet.
