# usage-store

How much of each subscription's limit is used, and what each Anthropic API key
has cost. It runs inside llm-bridge-server, mounted at `/usage/` with the
paths below behind the prefix, as operator routes. The server's one-shot router
reads `GET /api/usage/limits` in process before each routed call, and skips a
model whose subscription has a fresh window at 100%.

Records live in the Postgres module `usage` (role `usage_module`): limit
snapshots, spend keys, daily spend, top-ups, and the spend snapshots an older
version wrote. `llm-bridge-server import-usage -sqlite <file>` copies a
standalone usage-store's SQLite file into the module, keeping every id.

## Packages

- `usagestore`: the store.
- `anthropic`: reads Claude's OAuth subscription usage from
  `https://api.anthropic.com/api/oauth/usage` with the token in
  `~/.claude/.credentials.json`.
- `codex`: starts `codex app-server` (the Codex CLI's JSON-RPC interface) for
  each read and asks it `account/rateLimits/read`. The CLI holds and refreshes
  its own login, so usage-store holds no OpenAI token.
- `limitsrefresh`: decides when each provider is read.
- `spend`: reads per-key spend from Anthropic's admin API; `spendrefresh` finds
  the admin key in auth-store and reads spend on an interval.
- `httpapi`: the routes.

## Routes

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/usage` | Token totals (`day` / `week` / `month`) from model-store's old daily usage rows, which nothing writes now |
| GET | `/api/usage/limits` | Latest snapshot for every provider |
| GET | `/api/usage/limits/{provider}` | Latest snapshot for one provider |
| GET | `/api/usage/limits/{provider}/history?window=&since=&until=&limit=` | Snapshot history (for graphing) |
| POST | `/api/usage/limits/refresh?provider=anthropic\|codex` | Read one provider now and keep the snapshot |
| POST | `/api/usage/limits/watch` | The usage page is open: read limits on the watched interval for a while. 204. |
| GET | `/api/usage/spend/keys` | Per-account, per-key spend with top-ups and remaining credit |
| GET | `/api/usage/spend/keys/{provider}/{api_key_id}/raw` | The provider's last answer about one key |
| POST | `/api/usage/spend/refresh?provider=anthropic` | Read per-key spend now |
| GET · POST | `/api/usage/spend/topups` | List (`?provider=`) or record credit bought |
| DELETE | `/api/usage/spend/topups/{id}` | Delete one top-up |
| GET | `/health` | Liveness |

## Settings and background reads

The settings are llm-bridge-server's, `usage_store.*` in
`internal/config/usage_and_eval_settings.go`, read at start.

- **Limits** are read in the background only when
  `usage.limits_refresh.enabled` is on (off by default). Each provider is
  read every `usage.limits_refresh.idle_interval` (15m), and every
  `watched_interval` (2m) while the usage page has called `/limits/watch` within
  `watcher_expiry` (3m). A failed read waits one watched interval, doubling up to
  the idle interval. Anthropic's usage endpoint shares its rate limit with every
  Claude Code process on the login, which is why nothing reads it faster.
  Codex limits need `usage.codex_command`.
- A snapshot is stale three idle intervals after it was taken; the routes set
  `stale_after` when they answer.
- **Spend** is read only when `usage.spend_refresh.enabled` is on: at
  start the server lists auth-store's Anthropic credentials (`auth_store.url`,
  `auth_store.token`), resolves each `api_key` one and takes the first whose key
  starts `sk-ant-admin`; every resolve shows in auth-store's audit trail. It
  reads every `usage.spend_refresh.interval` (1h). A model missing from
  `spend/pricing.go` is priced at $0 and logged.

## Snapshot rows

`limit_snapshots` rows are only ever added: one row per window per read, and a
provider's latest snapshot is the rows sharing its newest `snapshot_at`. Most
codex rows repeat the one before. The history route has no default page size,
so always send `limit` and `since`.
