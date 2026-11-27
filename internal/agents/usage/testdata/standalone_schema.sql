-- The tables of a standalone usage-store's SQLite file, as its start-up
-- migrations left them, with the spend_snapshots table an older version made.
CREATE TABLE usage (
	date TEXT NOT NULL, agent TEXT NOT NULL, orchestrator TEXT NOT NULL DEFAULT 'inber', model TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, requests INTEGER DEFAULT 0, cost_usd REAL DEFAULT 0,
	PRIMARY KEY (date, agent, orchestrator, model, session));
CREATE TABLE limit_snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT, snapshot_at INTEGER NOT NULL, provider TEXT NOT NULL, window_key TEXT NOT NULL,
	used_percent REAL NOT NULL, window_minutes INTEGER, resets_at INTEGER, plan_type TEXT, tier TEXT, source TEXT NOT NULL,
	raw_json TEXT);
CREATE TABLE spend_snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT, fetched_at INTEGER NOT NULL, provider TEXT NOT NULL, window TEXT NOT NULL,
	period_start INTEGER NOT NULL, period_end INTEGER NOT NULL, total_usd REAL NOT NULL, currency TEXT NOT NULL,
	source TEXT NOT NULL, raw_json TEXT);
CREATE TABLE spend_keys (
	provider TEXT NOT NULL, api_key_id TEXT NOT NULL, api_key_name TEXT NOT NULL DEFAULT '', api_key_hint TEXT NOT NULL DEFAULT '',
	api_key_status TEXT NOT NULL DEFAULT '', raw_json TEXT, fetched_at INTEGER NOT NULL, PRIMARY KEY (provider, api_key_id));
CREATE TABLE spend_daily (
	provider TEXT NOT NULL, api_key_id TEXT NOT NULL, date TEXT NOT NULL, total_usd REAL NOT NULL, fetched_at INTEGER NOT NULL,
	PRIMARY KEY (provider, api_key_id, date));
CREATE TABLE spend_topups (
	id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL, amount_usd REAL NOT NULL, occurred_at INTEGER NOT NULL,
	note TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
