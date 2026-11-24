# About project-store

## What it owns

`127.0.0.1:8320`, unit `project-store.service`. Projects: what each is for (name, kind `project` or `stream`, goal, what done means, stage, spend target, parent project) and what it owns in other stores — repos, boards, scheduler jobs, services, people, notes, filed sessions — each by the id its own store gave it. Ids are `project_000001`. It also files chat sessions under projects by itself (*The session filer*). `CONTRACT.md` is the route table.

It stores nothing that happens in a project. Cards, branches, commits, pull requests, sessions, deploys and spend are worked out on request (`/projects/{id}/rollup`, `/projects/{id}/activity`, `/digest`) from what the project owns and from the cards kanban-store links to it, so they are never stale here. Pull requests come from repo-store, which syncs them from GitHub; this service never calls GitHub.

**Next steps are noteboard todos linked to the project** with a `note` link, the same way a decision note is: noteboard holds what a step says and whether it is done, and `POST /projects/{id}/next-steps` makes the todo (tag `next-step`) and the link in one call. The rollup reads every note link's item back in one `POST /api/items/query`. **Suggested next steps are the one thing stored here that a project did not decide**: `POST /projects/{id}/suggestions` asks llm-bridge-server's one-shot router (`POST /oneshot`, role `PROJECT_STORE_SUGGESTION_MODEL_ROLE`, default `balanced`, caller `project-store-next-step-suggestions`) to read the project's digest, open todos, cards, pull requests and last 14 days of activity, and keeps each proposal (`suggestion_000001`) until a person accepts it — it becomes a next step — or dismisses it. A new round supersedes the undecided proposals of the last.

# How it works

## Boards are views, projects are scope

A project may own several boards and a board may show cards from several projects: a team board keeps its own columns, and each column maps to kanban-store's shared `work_state` (`not_started`, `working`, `waiting`, `done`, `dropped`), which is what a rollup counts. A card joins a project through a kanban-store card link with `entity_type` `project`, never through a table here. Nesting belongs to projects (`parent_id`) and to cards (noteboard's `parent_id`); boards stay flat.

## Every link is checked with its owner

`POST /projects/{id}/links` asks the owning store for the id before writing it (`owners.go`) and keeps the store's name as `label`, for display only: nothing reads a label back to find a row. The owner saying no is 400, the owner not answering is 502, and nothing is written in either case. kanban-store needs `KANBAN_STORE_SERVICE_TOKEN` and llm-bridge-server `LLMBRIDGE_SERVICE_TOKEN`, which the service reads from a host-local drop-in (`~/.config/systemd/user/project-store.service.d/`) and refuses to start without.

## The session filer

**Chats are filed under projects by a pass that a scheduler job runs** (`POST /session-filings/run`, `session_filer.go`); nothing in llm-bridge-server tells this service that a brief was written, so the job asks every few minutes, and a backfill is the same call with `since_days`. Which job runs it on a host, and what its first backfill did, is in the noteboard note tagged `session-filer`. A pass lists the chats (type `interactive`, purpose `chat`) llm-bridge-server changed in the window and leaves alone: a session anyone but the filer filed, since a person's filing stands; a session started by another session or found on disk outside the bridge; and a session it already judged, unless its brief records a changed goal since, or it was judged without a brief and now has one, or its proposal still waits and a project has been made since.

For each of the rest it makes one model call through llm-bridge-server's one-shot router (role `PROJECT_STORE_SESSION_FILER_MODEL_ROLE`, default `balanced`, caller `project-store-session-filer`). The model reads every live project (id, goal, the names repo-store has for its repos) and the chat: its brief's goal and current step from `GET /session-briefs`, its name, its folder, and the repos it moved refs in from work-graph-store's `GET /session-repos`. It answers with a project id from the list or none, its odds, and a sentence of why; the schema allows only the listed ids, and an id that was not shown is an error, so **the filer never makes or names a project that does not exist**. Odds at or above `PROJECT_STORE_SESSION_FILER_MINIMUM_CONFIDENCE` (default 0.85) file the session, with `created_by` `project-store session filer`. Anything less is a `proposed` filing that waits for a person (`GET /session-filings?status=proposed`, then accept or dismiss). A chat with no brief is judged from its name and repos once it has been unchanged for `PROJECT_STORE_SESSION_FILER_QUIET_BEFORE_FILING_WITHOUT_A_BRIEF` (default 1h), because a chat gets its brief only when its first turn ends.

**Every judgment is a row of `session_filings`** (`session_filing_000001`), never rewritten to a new verdict: it is how a link the filer made is traced to its evidence, and how the filer knows what it has judged. Deleting the filer's link does not make it file the session again; only a changed goal does. When a pass cannot judge some session it answers 502 with its report, so the job's run shows as failed, and the next pass tries that session again.

# Working in this repo

## Tests and deploys

The wire types are rendered to TypeScript in `ts/model.ts` (`@kayushkin/project-store-types`, which bridge-ui links) by `./generate-ts.sh` from the files `tygo.yaml` names; run it after changing a wire type and commit the result. The list answers (`{"projects":[…]}`, `{"links":[…]}`) are unnamed maps in `server.go`, so they are not rendered; a caller wraps the named element type. `store.go` holds some internals too, and tygo renders them; nothing reads them.

`go test ./...`; `server_test.go` runs every route against fake owners. Deploy only with `./deploy.sh`, which refuses when the token drop-in is missing and smoke-checks `/digest`, the call that reaches every owner.
