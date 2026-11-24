# About usage-store

## What it is

usage-store answers two questions for this host: how much of each subscription's limit is used, and what each API key has cost. It is a Go library (`github.com/kayushkin/usage-store`, package `usagestore`) and a service, `usage-store-server`, that serves the library over HTTP on `:8185`. It owns the `limit_snapshots`, `spend_keys`, `spend_daily` and `spend_topups` tables in `~/.config/usage-store/usage.db`. `README.md` holds the route table and the settings table; keep them true when you change either.

## Who calls it

dash proxies it (`dash/server/api_usage.go`) for the usage page, which bridge-ui draws (`BridgeUsage.tsx`). The scheduler's autoworker reads `GET /api/usage/limits` before it dispatches work, and skips a provider whose snapshot is stale. healthcheck watches the `usage-store` unit with `auto_restart`. No `go.mod` under `~/repos` requires the library (measured 2026-09-20): only this repo's own server uses it.

## The token-accounting half is dead

`Track`, `Query`, `Stats`, `Summary` and `TotalCost` have no caller outside this repo's tests. `GET /api/usage` reads the `usage` table in model-store's `~/.config/model-store/store.db`, read-only, and the newest row there is dated 2026-04-26. The `usage` table in this service's own database has never been written. Do not build on either until noteboard todo `c211d55a-21fb-4a03-927a-099b62055da4` is decided. Live per-request accounting is not here.

# How it works

## Subscription limits

`internal/limitsrefresh` reads every provider each `USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL` (15m). While the usage page is open it calls `POST /api/usage/limits/watch` about once a minute, and reads speed up to every 2m until 3m pass with no call. A failed read backs off, doubling up to the idle interval. Nothing may poll faster: Anthropic's usage endpoint shares its rate limit with every Claude Code process on the login.

- `anthropic` reads `https://api.anthropic.com/api/oauth/usage` with the OAuth token in `~/.claude/.credentials.json`.
- `codex` starts `codex app-server` on every read and asks it `account/rateLimits/read`. The Codex CLI holds and refreshes the login; usage-store holds no OpenAI token. It needs `USAGE_STORE_CODEX_COMMAND`, and the npm `codex` needs `node` on `PATH` too — the unit's `PATH` line is there for that. With the variable unset or the command missing, codex limits are off and the start-up log says so. It no longer reads `~/.codex/sessions/`.

The server sets `StaleAfter` when it answers, to the snapshot time plus three idle intervals (45m).

## Snapshots are append-only and the table is large

Each `SaveLimits` call writes one row per window and nothing prunes them. Measured 2026-09-20: `usage.db` is 647 MB, 585 MB of it `limit_snapshots` (about 540,000 rows since April). Most codex rows repeat the one before — noteboard todo `ca905a20-4a1b-4c06-a917-236146ddd815`. The history route has no default page size, so always send `limit` and `since` (todo `4c70597a-887c-480d-ae31-2bcbd229334d`). Never copy or dump the live database in a test; `scripts/e2e-smoke.sh` shows how to run against temp ones.

## Spend per API key

Spend is off unless `AUTH_STORE_TOKEN` is set. On this host it is set in the host-local drop-in `~/.config/systemd/user/usage-store.service.d/spend.conf` — keep it mode 600, and never put the token in the tracked unit file. At start the server lists auth-store's credentials, resolves each Anthropic `api_key` credential, and takes the first whose key starts `sk-ant-admin`; every resolve shows in auth-store's audit trail on purpose. It refreshes hourly (`USAGE_STORE_SPEND_REFRESH_INTERVAL`). A model missing from `spend/pricing.go` is priced at $0 and logged, so add new models there. Top-ups are rows a person adds through `/api/usage/spend/topups`.

# Access and operations

## Live service

`usage-store.service` is a user unit running `~/bin/usage-store-server`. It listens on every interface at `:8185` with no auth, so treat every route as public to the network. `GET /health` is liveness only. Logs: `journalctl --user -u usage-store`. The start-up line names both database paths, both refresh intervals and whether codex and spend are on — read it before you guess at configuration. `~/.config/usage-store/limits.db` is an empty leftover; nothing opens it.

## Settings

`internal/config` declares every environment variable once with `llm-bridge/servicesettings`, `main` reads its configuration from that registry, and `GET /settings` serves it: ungated like every other route, read-only, with `AUTH_STORE_TOKEN` shown only as set or unset. To add a variable, add its `Definition` there; a test fails on any `os.Getenv` the declarations do not name. The service refuses to start on a duration that does not parse, and on a set variable under an owned prefix that nobody declared. The owned prefixes stop short of `USAGE_STORE_` because `USAGE_STORE_URL` is how other services find this one. `deploy.sh` builds the registry from the running service's environment before it stops it. The build reads `../llm-bridge` through a `replace`, so the deploy gate checks that tree too.

# Working in this repo

## Build, test and deploy

`go build ./... && go vet ./... && go test ./...` — green on `main` on 2026-09-20. `scripts/e2e-smoke.sh` builds the server, boots it on a temp port against temp databases with `HOME` pointed away, and calls every route; run it after you add or change a route, because a clashing `ServeMux` pattern compiles and then panics at start. Put the mise shims on `PATH` for Go: `export PATH="$HOME/.local/share/mise/shims:$PATH"`.

Deploy with `./deploy.sh` from the main clone, on a clean, pushed `main`; it runs the shared deploy gate first and records the deploy in repo-store's ledger last. The gate was added on 2026-09-20 and no deploy has run since, so the ledger has no row for this repo yet. The running binary (built 2026-09-16) carries no `vcs.revision` at all, so no commit describes it. `deploy.sh` now refuses to install a binary like that, which is why you cannot deploy from a worktree. The script stops the service before it copies the binary, so a failed start leaves the usage page and the autoworker's limit check down until you fix it.

## Parked branches

On 2026-09-25 the nightly todo-worker landed all ten parked `fix/`, `test/` and `guard/` branches on `main` and deleted them, locally and on origin. They had diverged from `main`, so they were cherry-picked, not fast-forwarded. `ab08d64` had already rewritten the codex reader with the same position keying, so only the `internal/server` half of `97cc21b` went in. Before you start a branch, run `git branch -a --no-merged main`, and read any branch it lists before you redo its work.
