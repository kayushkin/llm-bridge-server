-- The tables of a standalone noteboard's SQLite file, as the standalone store's
-- migrate() left them (its FTS table and triggers aside), for the import tests.
CREATE TABLE items (
			id          TEXT PRIMARY KEY,
			type        TEXT NOT NULL,
			title       TEXT NOT NULL,
			body        TEXT DEFAULT '',
			tags        TEXT DEFAULT '[]',
			priority    INTEGER DEFAULT 0,
			rank        REAL DEFAULT 0,
			status      TEXT DEFAULT 'open',
			list_id     TEXT DEFAULT '',
			due_at      DATETIME,
			parent_id   TEXT,
			links       TEXT DEFAULT '[]',
			created_by  TEXT DEFAULT '',
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL
		, deleted_at DATETIME, schedule TEXT, held_at DATETIME, hold_reason TEXT DEFAULT '', auto_hold_at_usd REAL);
CREATE INDEX idx_items_type ON items(type);
CREATE INDEX idx_items_status ON items(status);
CREATE INDEX idx_items_list ON items(list_id);
CREATE INDEX idx_items_created_by ON items(created_by);
CREATE INDEX idx_items_deleted ON items(deleted_at);
CREATE TABLE item_revisions (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			item_id     TEXT NOT NULL,
			title       TEXT NOT NULL,
			body        TEXT DEFAULT '',
			tags        TEXT DEFAULT '[]',
			status      TEXT DEFAULT 'open',
			priority    INTEGER DEFAULT 0,
			list_id     TEXT DEFAULT '',
			parent_id   TEXT,
			links       TEXT DEFAULT '[]',
			deleted_at  DATETIME,
			reason      TEXT NOT NULL,
			replaced_at DATETIME NOT NULL
		);
CREATE INDEX idx_item_revisions_item ON item_revisions(item_id, id DESC);
CREATE INDEX idx_items_scheduled ON items(id) WHERE schedule IS NOT NULL;
CREATE INDEX idx_items_held ON items(id, hold_reason) WHERE held_at IS NOT NULL;
CREATE INDEX idx_items_spend_ceiling ON items(id) WHERE auto_hold_at_usd IS NOT NULL;
