# usage-store

Token usage tracking + subscription-limit aggregator. Library + service.

## Library

`github.com/kayushkin/usage-store` exposes `Open`, `Track`, `Query`, `Stats`, `Summary`, `TotalCost` for per-request token accounting (used by `model-store`), plus `SaveLimits`, `LatestLimits`, `HistoryLimits` for subscription-limit snapshots.

Sub-packages:
- `anthropic` — fetches Claude OAuth subscription usage from `https://api.anthropic.com/api/oauth/usage` using the token in `~/.claude/.credentials.json`.
- `codex` — starts `codex app-server` (the Codex CLI's JSON-RPC interface) each refresh and asks it `account/rateLimits/read`. The CLI answers from OpenAI with its own login and refreshes that login itself, so usage-store holds no token, and the numbers cover every Codex client on the account, not only sessions run on this host. Needs `USAGE_STORE_CODEX_COMMAND` (a path or a name on `PATH`; the npm `codex` also needs `node` on `PATH`) — unset or not found, codex limits are off and the startup log says so. `USAGE_STORE_CODEX_TIMEOUT` bounds one read (default 30s).

## Service (`usage-store-server`)

HTTP front for the library. Runs as a user systemd unit on `:8185` (default).

Endpoints (all under `/api/usage`):

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/usage` | Token aggregates (`day` / `week` / `month`) — same shape dash already consumes |
| GET | `/api/usage/limits` | Latest snapshot for every provider |
| GET | `/api/usage/limits/{provider}` | Latest snapshot for one provider |
| GET | `/api/usage/limits/{provider}/history?window=&since=&until=&limit=` | Snapshot history (for graphing) |
| POST | `/api/usage/limits/refresh?provider=anthropic\|codex` | Force a fresh fetch + persist |
| POST | `/api/usage/limits/watch` | The usage page is open: read limits on the watched interval for `USAGE_STORE_LIMITS_WATCHER_EXPIRY`. 204. |
| GET | `/health` | Liveness |
| GET | `/settings` | Every environment variable the service reads, the value in force and where it came from. `AUTH_STORE_TOKEN` shows only whether it is set. Read-only: there is no `PUT`. |

The limits refresher (`internal/limitsrefresh`) reads each provider every `USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL` (15m) and writes a row per (provider, window). While the usage page is open and visible it calls `POST /api/usage/limits/watch` about once a minute, and reads speed up to every `USAGE_STORE_LIMITS_WATCHED_REFRESH_INTERVAL` (2m) until `USAGE_STORE_LIMITS_WATCHER_EXPIRY` (3m) passes without a call. Anthropic's usage endpoint shares its rate limit with every Claude Code process on the login, which is why nothing polls it faster. A failed read waits one watched interval, doubling on each further failure up to the idle interval; a success clears it.

### Env vars

| Var | Default |
|---|---|
| `USAGE_STORE_LISTEN_ADDR` | `:8185` |
| `USAGE_STORE_DB` | `~/.config/usage-store/usage.db` (limit snapshots) |
| `USAGE_STORE_TOKENS_DB` | `~/.config/model-store/store.db` (token-usage source, read-only) |
| `USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL` | `15m` |
| `USAGE_STORE_LIMITS_WATCHED_REFRESH_INTERVAL` | `2m` |
| `USAGE_STORE_LIMITS_WATCHER_EXPIRY` | `3m` |
| `USAGE_STORE_CODEX_COMMAND` | unset (codex limits off) |
| `USAGE_STORE_CODEX_TIMEOUT` | `30s` |
| `USAGE_STORE_SPEND_REFRESH_INTERVAL` | `1h` |
| `AUTH_STORE_URL` | `http://127.0.0.1:8303` |
| `AUTH_STORE_TOKEN` | unset (spend collection off) |

They are declared once, in `internal/config` with `llm-bridge/servicesettings`, and `GET /settings` serves the list. The service refuses to start on an interval or timeout that is not a Go duration, and on a set variable that starts `USAGE_STORE_LISTEN`, `_DB`, `_TOKENS`, `_LIMITS`, `_CODEX` or `_SPEND` and is not in this table: that is a misspelling. `USAGE_STORE_URL`, which other services use to find this one, is not refused. A test fails on any `os.Getenv` the declarations do not name.

### Deploy

```bash
./deploy.sh
```

Builds the binary, installs to `~/bin/usage-store-server`, drops the systemd unit at `~/.config/systemd/user/usage-store.service`, and starts it.

## Snapshot schema

`limit_snapshots` rows are append-only. Each `SaveLimits` call writes one row per window; reconstruct a `ProviderLimits` with `LatestLimits(provider)` (returns the most recent `snapshot_at` group). `StaleAfter` is set by the server at read time, for every provider, to `snapshot_at` plus three idle intervals (45m by default): three missed idle reads in a row. The autoworker skips a stale snapshot.
