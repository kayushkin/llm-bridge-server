package localmail

import (
	"database/sql"
	"fmt"
)

// ============================ schema migrations ============================
//
// The same scheme as accounts/migrate.go: SQLite's user_version pragma counts
// the migrations a database has run. Each runs once, in order, inside a
// transaction, and a database written by a newer binary is refused rather than
// downgraded.
//
// The first entry is the schema this package created before it had migrations,
// written with IF NOT EXISTS so that a database made by that older binary —
// which sits at user_version 0 with the table already present — runs it as a
// no-op and moves on to the second.

// migrations are applied in order. Append only; never edit or reorder an entry
// that has shipped, because a database that already ran it will not run it
// again and the two would silently diverge.
var migrations = []struct {
	name string
	stmt string
}{
	{name: "0001_local_messages", stmt: localMessagesSchema},
	{name: "0002_split_bodies_and_imports", stmt: splitBodiesAndImportsSchema},
	{name: "0003_revisions_and_attachments", stmt: revisionsAndAttachmentsSchema},
}

const localMessagesSchema = `
CREATE TABLE IF NOT EXISTS local_messages (
	account_id     TEXT NOT NULL,
	id             TEXT NOT NULL,
	folder_id      TEXT NOT NULL DEFAULT 'INBOX',
	message_id     TEXT NOT NULL DEFAULT '',
	thread_id      TEXT NOT NULL DEFAULT '',
	in_reply_to    TEXT NOT NULL DEFAULT '',
	references_ids TEXT NOT NULL DEFAULT '[]',
	subject        TEXT NOT NULL DEFAULT '',
	from_name      TEXT NOT NULL DEFAULT '',
	from_email     TEXT NOT NULL DEFAULT '',
	to_addrs       TEXT NOT NULL DEFAULT '[]',
	cc_addrs       TEXT NOT NULL DEFAULT '[]',
	date_ms        INTEGER NOT NULL,
	snippet        TEXT NOT NULL DEFAULT '',
	body_text      TEXT NOT NULL DEFAULT '',
	body_html      TEXT NOT NULL DEFAULT '',
	unread         INTEGER NOT NULL DEFAULT 1,
	flagged        INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	PRIMARY KEY (account_id, id)
);

-- Listing is always newest-first within one folder, and the cursor walks that
-- same order, so the index carries the whole ORDER BY.
CREATE INDEX IF NOT EXISTS idx_local_messages_listing
	ON local_messages (account_id, folder_id, date_ms DESC, id DESC);

-- Threading lookups by RFC identity.
CREATE INDEX IF NOT EXISTS idx_local_messages_message_id
	ON local_messages (account_id, message_id);
`

// splitBodiesAndImportsSchema moves bodies out of the metadata row and adds
// what an imported export carries.
//
// Bodies move because SQLite stores a row's columns in order and spills a long
// value onto overflow pages: with body_text and body_html in the middle of the
// row, reading the unread flag after them walks every page of both bodies. A
// list or a batch resolve of metadata should cost the same whether a message is
// one line or a 2 MB newsletter.
//
// A body column is NULL when the message has no such representation and ”
// when it has one that is empty; the caller of GET .../body needs to tell those
// apart. Before this migration ” meant "not given", because Append refused a
// message with neither body, so existing blanks become NULL.
//
// text_bytes and html_bytes sit on the metadata row so that a metadata read can
// say which bodies exist and how large they are without opening the body table.
const splitBodiesAndImportsSchema = `
CREATE TABLE local_message_bodies (
	account_id TEXT NOT NULL,
	id         TEXT NOT NULL,
	body_text  TEXT,
	body_html  TEXT,
	PRIMARY KEY (account_id, id)
);
INSERT INTO local_message_bodies (account_id, id, body_text, body_html)
	SELECT account_id, id, NULLIF(body_text, ''), NULLIF(body_html, '') FROM local_messages;

ALTER TABLE local_messages ADD COLUMN text_bytes INTEGER;
ALTER TABLE local_messages ADD COLUMN html_bytes INTEGER;
UPDATE local_messages SET
	text_bytes = NULLIF(length(CAST(body_text AS BLOB)), 0),
	html_bytes = NULLIF(length(CAST(body_html AS BLOB)), 0);
ALTER TABLE local_messages DROP COLUMN body_text;
ALTER TABLE local_messages DROP COLUMN body_html;

ALTER TABLE local_messages ADD COLUMN bcc_addrs           TEXT NOT NULL DEFAULT '[]';
ALTER TABLE local_messages ADD COLUMN provider_message_id TEXT NOT NULL DEFAULT '';
ALTER TABLE local_messages ADD COLUMN source_revision     TEXT NOT NULL DEFAULT '';
ALTER TABLE local_messages ADD COLUMN outlook_store_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE local_messages ADD COLUMN outlook_folder_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE local_messages ADD COLUMN outlook_folder_path TEXT NOT NULL DEFAULT '';
ALTER TABLE local_messages ADD COLUMN revision            INTEGER NOT NULL DEFAULT 1;
ALTER TABLE local_messages ADD COLUMN content_sha256      TEXT NOT NULL DEFAULT '';
ALTER TABLE local_messages ADD COLUMN updated_at          INTEGER NOT NULL DEFAULT 0;
UPDATE local_messages SET updated_at = created_at;

-- The idempotency key of an import. Partial, because an appended message has
-- no provider id and many of them share the empty string.
CREATE UNIQUE INDEX idx_local_messages_provider_message_id
	ON local_messages (account_id, provider_message_id) WHERE provider_message_id <> '';

-- One row per export file (or any other unit a caller imports at once), so an
-- importer can skip a source whose bytes it has already imported without
-- parsing it again.
CREATE TABLE local_import_sources (
	account_id    TEXT NOT NULL,
	source_ref    TEXT NOT NULL,
	source_sha256 TEXT NOT NULL,
	message_count INTEGER NOT NULL,
	imported_at   INTEGER NOT NULL,
	PRIMARY KEY (account_id, source_ref)
);
`

// revisionsAndAttachmentsSchema keeps what an update replaces, and the
// attachments of every revision.
//
// The current revision stays where it was — local_messages and
// local_message_bodies — so every read of the present costs what it did. A
// revision an import replaces is copied here first, whole: its metadata as
// StoredMessage JSON, as GET .../metadata served it at the time, and both
// bodies with absent kept distinct from empty.
//
// Attachments are keyed by revision, so an old revision keeps the files it had
// without a copy. The bytes are in file-store; file_id is its id, and it never
// leaves this service, because file-store judges no caller and a route that
// took a file id alone would turn "may this person read this message?" into
// "can they guess an id?".
const revisionsAndAttachmentsSchema = `
CREATE TABLE local_message_revisions (
	account_id        TEXT NOT NULL,
	id                TEXT NOT NULL,
	revision          INTEGER NOT NULL,
	metadata_json     TEXT NOT NULL,
	body_text         TEXT,
	body_html         TEXT,
	replaced_at       INTEGER NOT NULL,
	PRIMARY KEY (account_id, id, revision)
);

CREATE TABLE local_message_attachments (
	account_id           TEXT NOT NULL,
	id                   TEXT NOT NULL,
	revision             INTEGER NOT NULL,
	attachment_id        TEXT NOT NULL,
	position             INTEGER NOT NULL,
	source_attachment_id TEXT NOT NULL DEFAULT '',
	filename             TEXT NOT NULL,
	content_type         TEXT NOT NULL,
	size_bytes           INTEGER NOT NULL,
	sha256               TEXT NOT NULL,
	content_id           TEXT NOT NULL DEFAULT '',
	is_inline            INTEGER NOT NULL DEFAULT 0,
	file_id              TEXT NOT NULL,
	PRIMARY KEY (account_id, id, revision, attachment_id)
);
`

func migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("local mail db is at schema version %d but this binary knows only %d migrations; refusing to run against a newer schema",
			current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		m := migrations[i]
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(m.stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump user_version after %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.name, err)
		}
	}
	return nil
}
