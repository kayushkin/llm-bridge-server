-- The schema of a standalone kanban-store's SQLite file after every one of its
-- own migrations, as sqlite3 .schema prints it. The import test builds a file
-- from it and fills it with made-up rows.
CREATE TABLE boards (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT DEFAULT '',
			archived    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL
		, business_hours TEXT, default_principal_id TEXT, default_agent_id TEXT, default_instance_id TEXT, classifier TEXT, default_bundle_id TEXT, assignment_pool TEXT, organization_id TEXT);
CREATE INDEX idx_boards_archived ON boards(archived);
CREATE TABLE columns (
			id          TEXT PRIMARY KEY,
			board_id    TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			name        TEXT NOT NULL,
			position    REAL NOT NULL,
			color       TEXT DEFAULT '',
			wip_limit   INTEGER,
			auto_status TEXT,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL
		, budget_clock_state TEXT, lifecycle_state TEXT, work_state TEXT);
CREATE INDEX idx_columns_board ON columns(board_id, position);
CREATE TABLE placements (
			card_id     TEXT NOT NULL,
			board_id    TEXT NOT NULL REFERENCES boards(id)  ON DELETE CASCADE,
			column_id   TEXT NOT NULL REFERENCES columns(id) ON DELETE CASCADE,
			position    REAL NOT NULL,
			created_at  DATETIME NOT NULL,
			updated_at  DATETIME NOT NULL,
			PRIMARY KEY (card_id, board_id)
		);
CREATE INDEX idx_placements_board   ON placements(board_id, column_id, position);
CREATE INDEX idx_placements_column  ON placements(column_id, position);
CREATE INDEX idx_placements_card    ON placements(card_id);
CREATE TABLE card_links (
			id          TEXT PRIMARY KEY,
			card_id     TEXT NOT NULL,
			entity_type TEXT NOT NULL,
			entity_ref  TEXT NOT NULL,
			label       TEXT DEFAULT '',
			created_at  DATETIME NOT NULL,
			UNIQUE (card_id, entity_type, entity_ref)
		);
CREATE INDEX idx_card_links_card   ON card_links(card_id);
CREATE INDEX idx_card_links_entity ON card_links(entity_type, entity_ref);
CREATE TABLE entity_tags (
			entity_type TEXT NOT NULL,
			entity_ref  TEXT NOT NULL,
			tag         TEXT NOT NULL,
			created_at  DATETIME NOT NULL,
			PRIMARY KEY (entity_type, entity_ref, tag)
		);
CREATE INDEX idx_entity_tags_tag    ON entity_tags(tag);
CREATE INDEX idx_entity_tags_entity ON entity_tags(entity_type, entity_ref);
CREATE TABLE card_events (
			id             TEXT PRIMARY KEY,
			card_id        TEXT NOT NULL,
			board_id       TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			clock_state    TEXT NOT NULL,
			actor          TEXT NOT NULL DEFAULT '',
			summary        TEXT NOT NULL DEFAULT '',
			from_column_id TEXT NOT NULL DEFAULT '',
			to_column_id   TEXT NOT NULL DEFAULT '',
			note_id        TEXT NOT NULL DEFAULT '',
			detail         TEXT NOT NULL DEFAULT '',
			occurred_at    DATETIME NOT NULL,
			recorded_at    DATETIME NOT NULL
		, supersedes_event_id TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_card_events_card  ON card_events(card_id, occurred_at);
CREATE INDEX idx_card_events_board ON card_events(board_id, occurred_at);
CREATE TABLE card_notes (
			id         TEXT PRIMARY KEY,
			card_id    TEXT NOT NULL,
			board_id   TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT 'note',
			body       TEXT NOT NULL,
			actor      TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		, visibility TEXT NOT NULL DEFAULT 'internal');
CREATE INDEX idx_card_notes_card ON card_notes(card_id, created_at);
CREATE TABLE board_priority_levels (
			board_id       TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			priority_value INTEGER NOT NULL,
			label          TEXT NOT NULL,
			budget_seconds INTEGER, default_auto_hold_at_usd REAL,
			PRIMARY KEY (board_id, priority_value)
		);
CREATE TABLE card_assignments (
			card_id      TEXT NOT NULL,
			principal_id TEXT NOT NULL,
			assigned_by  TEXT NOT NULL DEFAULT '',
			created_at   DATETIME NOT NULL,
			PRIMARY KEY (card_id, principal_id)
		);
CREATE INDEX idx_card_assignments_principal ON card_assignments(principal_id);
CREATE TABLE message_triggers (
			id                     TEXT PRIMARY KEY,
			board_id               TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			name                   TEXT NOT NULL,
			event_kind             TEXT NOT NULL,
			to_column_id           TEXT NOT NULL DEFAULT '',
			priority_value         INTEGER,               -- NULL = any priority
			recipient_user_id      TEXT NOT NULL,
			recipient_display_name TEXT NOT NULL DEFAULT '',
			message_template       TEXT NOT NULL,
			enabled                INTEGER NOT NULL DEFAULT 1,
			created_at             TEXT NOT NULL,
			updated_at             TEXT NOT NULL
		);
CREATE INDEX idx_message_triggers_board ON message_triggers(board_id);
CREATE TABLE message_deliveries (
			id                TEXT PRIMARY KEY,
			trigger_id        TEXT NOT NULL REFERENCES message_triggers(id) ON DELETE CASCADE,
			board_id          TEXT NOT NULL,
			event_id          TEXT NOT NULL,
			card_id           TEXT NOT NULL,
			recipient_user_id TEXT NOT NULL,
			rendered_message  TEXT NOT NULL DEFAULT '',
			status            TEXT NOT NULL,
			error             TEXT NOT NULL DEFAULT '',
			room_id           TEXT NOT NULL DEFAULT '',
			matrix_event_id   TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL,
			UNIQUE (trigger_id, event_id)
		);
CREATE INDEX idx_message_deliveries_board ON message_deliveries(board_id, created_at DESC);
CREATE TABLE board_tag_rules (
			id                   TEXT PRIMARY KEY,
			board_id             TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			position             INTEGER NOT NULL,
			tags                 TEXT NOT NULL,
			default_principal_id TEXT,
			default_agent_id     TEXT,
			default_instance_id  TEXT,
			default_bundle_id    TEXT,
			created_at           DATETIME NOT NULL,
			updated_at           DATETIME NOT NULL,
			UNIQUE (board_id, position)
		);
CREATE INDEX idx_board_tag_rules_board ON board_tag_rules(board_id, position);
CREATE TABLE tickets (
			card_id                TEXT PRIMARY KEY,
			requester_principal_id TEXT NOT NULL,
			channel                TEXT NOT NULL,
			created_at             DATETIME NOT NULL,
			updated_at             DATETIME NOT NULL
		, source_entity_type TEXT NOT NULL DEFAULT '', source_entity_ref TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_tickets_requester ON tickets(requester_principal_id);
CREATE UNIQUE INDEX idx_card_events_supersedes
		ON card_events(supersedes_event_id) WHERE supersedes_event_id != '';
CREATE TABLE card_attachments (
			file_id     TEXT PRIMARY KEY,
			card_id     TEXT NOT NULL,
			visibility  TEXT NOT NULL DEFAULT 'internal',
			attached_by TEXT NOT NULL DEFAULT '',
			created_at  DATETIME NOT NULL
		);
CREATE INDEX idx_card_attachments_card ON card_attachments(card_id, created_at);
CREATE TABLE classification_identifiers (
			sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
			id         TEXT NOT NULL UNIQUE,
			kind       TEXT NOT NULL CHECK (kind IN ('axis', 'value')),
			board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			axis_id    TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		);
CREATE INDEX idx_classification_identifiers_board ON classification_identifiers(board_id);
CREATE TABLE classification_taxonomy_revisions (
			board_id        TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			revision        INTEGER NOT NULL,
			taxonomy        TEXT,
			semantic_digest TEXT NOT NULL DEFAULT '',
			created_at      DATETIME NOT NULL,
			created_by      TEXT NOT NULL,
			PRIMARY KEY (board_id, revision)
		);
CREATE TABLE classification_taxonomy_migrations (
			board_id    TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			axis_name   TEXT NOT NULL,
			value_name  TEXT NOT NULL,
			assigned_id TEXT NOT NULL,
			migrated_at DATETIME NOT NULL,
			PRIMARY KEY (board_id, axis_name, value_name)
		);
CREATE TABLE classification_policy_revisions (
			board_id          TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			revision          INTEGER NOT NULL,
			axis_policies     TEXT NOT NULL,
			taxonomy_revision INTEGER NOT NULL,
			created_at        DATETIME NOT NULL,
			created_by        TEXT NOT NULL,
			PRIMARY KEY (board_id, revision)
		);
CREATE TABLE classification_decisions (
			id                        TEXT PRIMARY KEY,
			sequence                  INTEGER NOT NULL UNIQUE,
			board_id                  TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			organization_id           TEXT NOT NULL,
			card_id                   TEXT NOT NULL,
			publication_key           TEXT NOT NULL UNIQUE,
			publication_digest        TEXT NOT NULL,
			operation_id              TEXT NOT NULL,
			checkpoint_id             TEXT NOT NULL,
			attempt_number            INTEGER NOT NULL,
			initiating_principal_id   TEXT NOT NULL,
			source_digest             TEXT NOT NULL,
			source_revision           TEXT NOT NULL DEFAULT '',
			source_snapshot           TEXT,
			source_removed_at         DATETIME,
			taxonomy_revision         INTEGER NOT NULL,
			taxonomy_digest           TEXT NOT NULL,
			policy_revision           INTEGER NOT NULL,
			prompt_revision           TEXT NOT NULL,
			model                     TEXT NOT NULL,
			origin                    TEXT NOT NULL,
			selections                TEXT NOT NULL,
			rationale                 TEXT NOT NULL DEFAULT '',
			model_reported_confidence REAL,
			needs_review              INTEGER NOT NULL,
			review_reasons            TEXT NOT NULL,
			completed_at              DATETIME NOT NULL,
			published_at              DATETIME NOT NULL,
			FOREIGN KEY (board_id, taxonomy_revision) REFERENCES classification_taxonomy_revisions(board_id, revision),
			FOREIGN KEY (board_id, policy_revision) REFERENCES classification_policy_revisions(board_id, revision)
		);
CREATE INDEX idx_classification_decisions_board     ON classification_decisions(board_id, sequence);
CREATE INDEX idx_classification_decisions_card      ON classification_decisions(board_id, card_id, sequence);
CREATE INDEX idx_classification_decisions_operation ON classification_decisions(board_id, operation_id);
CREATE INDEX idx_classification_decisions_source    ON classification_decisions(card_id);
CREATE TABLE classification_reviews (
			id                        TEXT PRIMARY KEY,
			decision_id               TEXT NOT NULL REFERENCES classification_decisions(id) ON DELETE CASCADE,
			board_id                  TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			card_id                   TEXT NOT NULL,
			reviewer_principal_id     TEXT NOT NULL,
			action                    TEXT NOT NULL,
			axis_ids                  TEXT NOT NULL,
			corrections               TEXT,
			explanation               TEXT NOT NULL DEFAULT '',
			supersedes_review_ids     TEXT NOT NULL,
			expected_labels_revision  INTEGER NOT NULL,
			resulting_labels_revision INTEGER NOT NULL,
			taxonomy_revision         INTEGER NOT NULL,
			policy_revision           INTEGER NOT NULL,
			idempotency_key           TEXT NOT NULL,
			request_digest            TEXT NOT NULL,
			created_at                DATETIME NOT NULL,
			UNIQUE (board_id, reviewer_principal_id, idempotency_key)
		);
CREATE INDEX idx_classification_reviews_decision ON classification_reviews(decision_id, created_at);
CREATE TABLE classification_labels (
			board_id           TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			card_id            TEXT NOT NULL,
			axis_id            TEXT NOT NULL,
			value_ids          TEXT NOT NULL,
			origin_decision_id TEXT NOT NULL,
			origin_review_id   TEXT NOT NULL,
			updated_at         DATETIME NOT NULL,
			PRIMARY KEY (board_id, card_id, axis_id)
		);
CREATE TABLE classification_label_revisions (
			board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			card_id    TEXT NOT NULL,
			revision   INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (board_id, card_id)
		);
