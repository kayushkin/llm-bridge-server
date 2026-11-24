# repo-store

A small Go service that keeps the canonical registry of **repositories and
their detected signature** — the stable answer to *"what is this repo?"*
(languages, frameworks, build tools, test tools) as a flat set of tags — plus
a ledger of every deploy and a copy of each repo's GitHub pull requests.
SQLite behind a plain HTTP API.

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. Its tags are an input to the bundle resolver in
[bundle-store](https://github.com/kayushkin/bundle-store): a session's bundle of
skills and tools is chosen from the target repo's tags plus the task's.
Detection itself is the [repodetect](https://github.com/kayushkin/repodetect)
library, which repo-store serves over HTTP. The TypeScript types of the wire
format are in `ts/` as `@kayushkin/repo-store-types`, and *HTTP API* below
lists every route.

## Why this exists / boundary vs forge

`forge` owns **ephemeral, session-scoped** materializations — worktree slots, builds,
previews, containers ("a working copy for *this run*"). repo-store owns **stable
canonical identity + classification** ("what this repo *is*"), independent of any
session. forge may *consume* repo-store (look up a repo's tags for a worktree); it
must never own the classification. No overlap.

Today the only repo classification in the codebase is `inber/engine/workflow_hooks.go`
`detectProject()` — a private, first-match (`go.mod` → `"go"`, stop) detector used to
pick build/format/test commands. It is coarse and inber-internal. repo-store's
detection (`detect.go`) is the shared, **collect-all-signals** replacement: a repo
with both `go.mod` and `package.json` (e.g. dash) reports *both*. inber should adopt
this as a consumer rather than keeping its own copy.

## Data model

```
repos(
  id, name UNIQUE, path, git_remote, description,
  provenance    TEXT,   -- 'ours' | 'vendored' | '' (unstated)
  tags          JSON,   -- detected signature
  tag_overrides JSON,   -- manual additive tags (curation escape hatch)
  created_at, updated_at
)
```

**Effective tags = union(tags, tag_overrides)** — computed on read, never stored.

**`github_url`** — `https://github.com/<owner>/<name>` when `git_remote` is on GitHub, else absent. Also computed on read: an SSH host alias (`git@gh-alias:…`) is resolved with `ssh -G`, as git would reach it, so an alias in `~/.ssh/config` counts.
Detection writes `tags`; humans add `tag_overrides` when detection misses something.

### Provenance — who authors a repository

`provenance` answers *"is this ours?"*: `ours`, `vendored`, or empty when nobody has
stated it. It exists because every fleet-wide count taken on a host — tests,
functions, flagged rows — sums over a repository set, and a set that silently
includes vendored upstream code overstates the work anyone here may actually do.
A number whose population is not stated is not a measurement; this is the field
that lets the population be stated. `GET /repos?provenance=ours` is the query a
fleet walk should ask instead of listing directories.

Three rules the code enforces, each with a test:

- **Never inferred.** The store records what it was told. An unstated provenance
  stays unstated rather than defaulting to `ours`, because a default would put a
  guess underneath every number taken over the registry.
- **Not written by the upsert path.** `POST /repos` is the routine tag refresh, and
  a refresh whose body omitted the provenance would otherwise blank it. The only
  write path is `PUT /repos/{id}/provenance`. A `POST` body carrying `provenance`
  is **refused with a 400** naming that path — accepting and ignoring it would
  answer 200 with the stored value, which reads exactly like the request applying.
- **The unstated class has its own query word.** `?provenance=unstated`. An empty
  parameter cannot mean it, because `?provenance=` is indistinguishable from the
  parameter being absent, and absent means the filter is off — so an empty value
  is a 400 rather than a third meaning.

## Detection (`detect.go`, the `Detect(path) → Signature` library)

Pure, deterministic, manifest-driven. Returns a structured `Signature`
(`languages`, `frameworks`, `build_tools`, `test_tools`); `Signature.Tags()` flattens
to a sorted/deduped tag list, with frontend frameworks rolling up to generic
`web`/`frontend`. Current signals:

- `go.mod` → `go`; `Cargo.toml` → `rust`; `pyproject.toml`/`requirements.txt`/`setup.py` → `python`
- `package.json` → `node`, then dep-based: `react`/`vue`/`svelte`/`next`/`angular`, `node-backend` (express/fastify/koa), `vite` (dep or `vite.config.*`), `typescript`, `playwright`/`cypress` → `+e2e`
- `tsconfig.json` → `typescript`; `Dockerfile` → `docker`

This `Detect` function is the **single source of the ruleset** — intended to be
imported directly (the library) or called over HTTP (`POST /detect`).

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` | liveness |
| GET | `/settings` | every environment variable the service reads, as llm-bridge `msg.ServiceSettings`: the value in force, its default and where it came from. Read-only: no setting is editable and `PUT /settings/{key}` is not mounted |
| GET | `/repos?tag=&q=&limit=&provenance=` | list (tag matches effective tags; `provenance` is `ours`, `vendored` or `unstated`) |
| POST | `/repos` | upsert by name; if `path` exists on disk, (re)detects & stores `tags`, keeps body `tag_overrides` |
| GET | `/repos/{id}` | fetch by id |
| GET | `/repos/by-name/{name}` | fetch by name |
| GET | `/repos/by-mention/{mention}` | fetch by a chat mention, `repo:<name>`; 404 for an unknown name, 400 for anything not a mention. The chat's reference resolver calls it through kanban-store's entity-type registry |
| GET | `/by-path?path=<abs>` | fetch by path (paths contain slashes) |
| POST | `/repos/{id}/detect` | re-run detection on the stored path, refresh tags |
| PUT | `/repos/{id}/provenance` | `{"provenance":"ours"\|"vendored"\|""}` — the only write path for the field |
| PUT | `/repos/{id}/identity` | `{"name":…,"path":…,"git_remote":…}`, all three required (send the current value to keep one): renames the repo and moves its path and remote **keeping its id**, so deploy-ledger rows stay attached. `POST /repos` cannot do this, because it upserts by name. 409 when another row holds the name or path; 400 for an empty name, a relative path, a missing or unknown field; 404 for an unknown id. Re-detects tags when the new path exists, keeps them when it does not. Touches nothing on disk. Ledger rows keep their `source_path` (history); their `repo_name` is joined on read, so it shows the new name |
| DELETE | `/repos/{id}` | remove |
| POST | `/detect` | **stateless**: `{path}` → `{signature, tags}`, no persistence |
| GET | `/pull-requests?repo_id=&state=&limit=` | `{"pull_requests":[…]}`, most recently updated first. `state` is `open` (default), `closed` (closed without merging), `merged` or `all`; `limit` defaults to 100; no `repo_id` means every repo. An unknown parameter or an empty value is a 400, an unknown `repo_id` a 404 |
| POST | `/repos/{id}/pull-requests/sync` | read the repo's pull requests from GitHub now, then answer as `GET /pull-requests?repo_id={id}&state=all`. 400 when the repo has no `github_url`, 502 with GitHub's error when GitHub fails |

```bash
# Detect without persisting (what the resolver calls for unregistered repos)
curl -s -X POST :8306/detect -d '{"path":"/srv/src/webapp"}'
# → {"signature":{...},"tags":["e2e","frontend","go","node","playwright","react","typescript","vite","web"]}

# Register a repo (auto-detects from path)
curl -s -X POST :8306/repos -d '{"name":"webapp","path":"/srv/src/webapp"}'

# State who authors it, then ask for the repos a fleet-wide count may include
curl -s -X PUT :8306/repos/42/provenance -d '{"provenance":"vendored"}'
curl -s ':8306/repos?provenance=ours'
curl -s ':8306/repos?provenance=unstated'   # what still needs an answer
```

## Decisions taken (in this scaffold)

- **Scope = canonical repo registry** (identity + signature + overrides), not just
  bundling tags — it can replace hand-kept lists of repos and back a dashboard's
  topology later. (Open: how far to take identity — remotes,
  deployed-service links.)
- **Compute-on-read for the stateless `/detect`; cache-on-write for registered repos**
  (`tags` persist until re-detected). No git-event invalidation yet.
- **Local repos only.** repo-store stats local paths. Remote-instance repos (other
  llm-bridge hosts) would need tags pushed in or a per-instance detector — deferred.

## ⚠️ There is no ingest — population is by hand, one repo at a time

Nothing here ever discovers a repository. There is no walk in the service, no
scan route, no cron job, and no seed script in this repo. A row exists only
because somebody sent `POST /repos` for it.

A registry populated once by walking a directory drifts in two ways: it gets
rows for directories that are not repositories, and it has no rows for
repositories created afterwards. Whether population should become an ingest
(and if so, whether reconciliation may delete rows) is an open design question.

This matters beyond a wrong count because of what joins here: bundle-store's
`/resolve` composes a session bundle from a repo's effective tags, so a
repository with no row has no tags and resolves to the empty-tag case silently.

```bash
scripts/registry-drift.sh [ROOT]      # report drift against ROOT (default ~/repos), read-only
scripts/registry-drift.sh --self-test # prove the walk before trusting a clean run
```

Note that `Detect` returns no tags for a repository with no manifest — a
single script with no `pyproject.toml` or `requirements.txt` is registered and
correctly untagged. That is the ruleset behaving as documented, not a missing
row.

## The deploy ledger

`deployments` holds one append-only row per deploy of a repo: the full commit
sha, the branch and tree it was built in, whether that commit is on
`origin/<default branch>`, whether the tree was dirty, and who ran it. The shared
deploy gate of the llm-bridge hosts (`deploy-gate`, kept in the healthcheck repo)
writes it after a `deploy.sh` succeeds and reads it before one starts, so the
next agent sees what is live before it builds.

| Route | |
|---|---|
| `POST /repos/{id}/deployments` | Record one. `commit_sha` must be the full 40 characters. `refusals` lists what the gate objected to; a non-empty list requires `override_reason`, and a reason with nothing refused is a 400. |
| `GET /deployments?repo_id=` | One repo's history, newest first (`limit`, default 50). |
| `GET /deployments/latest` | The newest deployment of every repo that has one. |

There is no PUT and no DELETE. This is durable fact about a repo, not the
session-scoped state that belongs to forge.

## Pull requests

repo-store is the one service that reads GitHub pull requests; project-store
and dash read them from here. `pull_requests` holds one row per (repo_id,
number), rewritten whenever GitHub reports a change and never deleted by the
sync. Times are unix seconds, 0 for one that has not happened. `state` is
`open`, `closed` or `merged`: GitHub says only open or closed, and a closed
pull request with a merge time is stored as merged.

The background sync runs at startup and then every
`REPO_STORE_PULL_REQUEST_SYNC_INTERVAL` (default `10m`). For each repo with a
`github_url` it asks GitHub's list endpoint (`state=all&sort=updated&direction=desc`,
100 a page) for pages until it reaches a pull request updated before that
repo's last successful sync began; `pull_request_syncs` keeps that time and the
GitHub repository it belongs to. A failed sync writes nothing and leaves the
cursor, so the next pass asks for the same span again.

It reaches GitHub by running `gh api` (`REPO_STORE_GITHUB_CLI`, set to
`~/bin/gh` in the unit), as the account gh is logged in to under the unit's
HOME. Each pass costs at least one request per repo against that account's
5,000 an hour.

```bash
curl -s ':8306/pull-requests?state=all&limit=5'
curl -s -X POST :8306/repos/15/pull-requests/sync | jq '.pull_requests | length'
journalctl --user -u repo-store | grep 'pull-request sync'
```

## Build

repo-store reads its settings through llm-bridge's `servicesettings` package
and detects with `repodetect`; `go.mod` points at checkouts of both beside
this one:

```sh
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/repodetect
git clone https://github.com/kayushkin/repo-store
cd repo-store
go test ./...
go build -o repo-store ./cmd/repo-store
```

`scripts/e2e-smoke.sh` builds the binary and drives the routes against a
fixture tree and a throwaway database; it never touches a running service.

## Run

```sh
./repo-store              # listens on 127.0.0.1:8306
curl -s localhost:8306/health
```

| Variable | Default | |
|---|---|---|
| `REPO_STORE_ADDR` | `127.0.0.1:8306` | listen address |
| `REPO_STORE_DATA_DIR` | `~/.config/repo-store` | holds `repo-store.db` |
| `REPO_STORE_PULL_REQUEST_SYNC_INTERVAL` | `10m` | time between pull-request syncs |
| `REPO_STORE_GITHUB_CLI` | `gh` | the gh program; the service will not start without it |

The variables are declared once in `settings.go`, and `GET /settings` describes
each one with the value in force. A set `REPO_STORE_` variable that is not
declared there stops the service at boot. Run `gh auth login` as the user the
service runs as before starting it, or every pull-request sync fails.

**There is no authentication.** Keep it on loopback, as the default does, and
put a front door that checks who is asking in front of it if anyone else
should reach it. `POST /detect` and `POST /repos` read any path the service's
user can read.

To run it as a systemd user service, copy the binary to `~/bin` and
`repo-store.service` to `~/.config/systemd/user/`, set
`REPO_STORE_GITHUB_CLI` in the unit to where gh is installed, then
`systemctl --user enable --now repo-store`. `deploy.sh` does this, with
checks, on a host that runs the llm-bridge deploy gate.

## License

MIT. See `LICENSE`.
