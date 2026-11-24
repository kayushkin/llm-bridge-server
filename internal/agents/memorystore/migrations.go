package memorystore

import (
	"database/sql"
)

// runMigrations applies schema migrations to existing databases
func runMigrations(db *sql.DB) error {
	// Get list of existing columns
	existingCols := make(map[string]bool)
	rows, err := db.Query("PRAGMA table_info(memories)")
	if err != nil {
		return err
	}
	defer rows.Close()
	
	var cid int
	var name, typ string
	var notnull, pk int
	var dflt sql.NullString
	
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		existingCols[name] = true
	}
	
	// List of all migrations (idempotent - run them all, sqlite will ignore errors for existing columns)
	migrations := []string{
		// Context unification fields (2026-03-01)
		"ALTER TABLE memories ADD COLUMN always_load INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE memories ADD COLUMN expires_at INTEGER",
		"ALTER TABLE memories ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0",
		"CREATE INDEX IF NOT EXISTS idx_always_load ON memories(always_load)",
		"CREATE INDEX IF NOT EXISTS idx_expires_at ON memories(expires_at)",
		
		// Reference fields for lazy loading (2026-03-01)
		"ALTER TABLE memories ADD COLUMN ref_type TEXT NOT NULL DEFAULT 'memory'",
		"ALTER TABLE memories ADD COLUMN ref_target TEXT",
		"ALTER TABLE memories ADD COLUMN is_lazy INTEGER NOT NULL DEFAULT 0",
		"CREATE INDEX IF NOT EXISTS idx_ref_type ON memories(ref_type)",

		// Orchestrator scoping (2026-03-21)
		"ALTER TABLE memories ADD COLUMN orchestrator TEXT NOT NULL DEFAULT ''",
		"CREATE INDEX IF NOT EXISTS idx_orchestrator ON memories(orchestrator)",

		// Phase II.B (2026-05-09): drop the dead sessions / session_tags
		// tables. memory-store no longer maintains its own session
		// metadata; per-session aggregates live in log-store's projection.
		// memory_usage's session_id is now an opaque reference to the
		// canonical session_id from llm-bridge-server.
		"DROP TABLE IF EXISTS session_tags",
		"DROP TABLE IF EXISTS sessions",
	}
	
	for _, migration := range migrations {
		if _, err := db.Exec(migration); err != nil {
			// Ignore errors for already-existing columns/indexes
			continue
		}
	}
	
	return nil
}