-- The schema of a standalone memory-store's SQLite file after its last
-- migration, as the store left it: the columns added by ALTER TABLE, the
-- foreign key to the dropped sessions table, and the FTS5 index.
CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		summary TEXT,
		original_id TEXT,
		importance REAL NOT NULL DEFAULT 0.5,
		access_count INTEGER NOT NULL DEFAULT 0,
		last_accessed INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		source TEXT NOT NULL,
		embedding BLOB
	, always_load INTEGER NOT NULL DEFAULT 0, expires_at INTEGER, tokens INTEGER NOT NULL DEFAULT 0, ref_type TEXT NOT NULL DEFAULT 'memory', ref_target TEXT, is_lazy INTEGER NOT NULL DEFAULT 0, orchestrator TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_importance ON memories(importance);
CREATE INDEX idx_last_accessed ON memories(last_accessed);
CREATE INDEX idx_created_at ON memories(created_at);
CREATE INDEX idx_source ON memories(source);
CREATE TABLE memory_tags (
		memory_id TEXT NOT NULL,
		tag TEXT NOT NULL,
		FOREIGN KEY (memory_id) REFERENCES memories(id) ON DELETE CASCADE
	);
CREATE INDEX idx_memory_tags_memory_id ON memory_tags(memory_id);
CREATE INDEX idx_memory_tags_tag ON memory_tags(tag);
CREATE TABLE memory_usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		turn_number INTEGER NOT NULL,
		usage_type TEXT NOT NULL,
		accessed_at INTEGER NOT NULL,
		FOREIGN KEY (memory_id) REFERENCES memories(id) ON DELETE CASCADE,
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
	);
CREATE INDEX idx_memory_usage_memory_id ON memory_usage(memory_id);
CREATE INDEX idx_memory_usage_session_id ON memory_usage(session_id);
CREATE INDEX idx_always_load ON memories(always_load);
CREATE INDEX idx_expires_at ON memories(expires_at);
CREATE INDEX idx_ref_type ON memories(ref_type);
CREATE INDEX idx_orchestrator ON memories(orchestrator);
CREATE VIRTUAL TABLE memories_fts USING fts5(
	memory_id UNINDEXED,
	content,
	summary,
	tokenize='porter unicode61'
)
;
