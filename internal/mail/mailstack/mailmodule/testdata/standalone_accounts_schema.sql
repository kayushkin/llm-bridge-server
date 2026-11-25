CREATE TABLE accounts (
	id               TEXT PRIMARY KEY,
	label            TEXT NOT NULL,
	provider         TEXT NOT NULL,
	address          TEXT NOT NULL,
	apiauth_provider TEXT NOT NULL DEFAULT '',
	apiauth_account  TEXT NOT NULL DEFAULT '',
	imap_cred_name   TEXT NOT NULL DEFAULT '',
	color            TEXT NOT NULL DEFAULT '#888',
	signature        TEXT NOT NULL DEFAULT '',
	enabled          INTEGER NOT NULL DEFAULT 1,
	sort_order       INTEGER NOT NULL DEFAULT 0,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);
CREATE TABLE imap_creds (
	name       TEXT PRIMARY KEY,
	address    TEXT NOT NULL,
	imap_host  TEXT NOT NULL,
	imap_port  INTEGER NOT NULL,
	smtp_host  TEXT NOT NULL,
	smtp_port  INTEGER NOT NULL,
	username   TEXT NOT NULL,
	password   TEXT NOT NULL,
	tls_mode   TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE imap_folder_map (
	account_id TEXT NOT NULL,
	canonical  TEXT NOT NULL,
	folder_id  TEXT NOT NULL,
	PRIMARY KEY (account_id, canonical)
);
CREATE TABLE messages (
	account_id      TEXT NOT NULL,
	folder_scope    TEXT NOT NULL DEFAULT '',
	provider_id     TEXT NOT NULL,

	message_id      TEXT NOT NULL DEFAULT '',
	thread_id       TEXT NOT NULL DEFAULT '',
	root_message_id TEXT NOT NULL DEFAULT '',
	in_reply_to     TEXT NOT NULL DEFAULT '',
	references_ids  TEXT NOT NULL DEFAULT '[]',

	subject         TEXT NOT NULL DEFAULT '',
	from_name       TEXT NOT NULL DEFAULT '',
	from_email      TEXT NOT NULL DEFAULT '',
	date_ms         INTEGER NOT NULL DEFAULT 0,
	folder_id       TEXT NOT NULL DEFAULT '',
	uid_validity    INTEGER NOT NULL DEFAULT 0,

	first_seen_ms   INTEGER NOT NULL,
	last_seen_ms    INTEGER NOT NULL,

	PRIMARY KEY (account_id, folder_scope, provider_id)
);
CREATE INDEX idx_messages_message_id ON messages(message_id) WHERE message_id != '';
CREATE INDEX idx_messages_thread ON messages(account_id, thread_id) WHERE thread_id != '';
CREATE INDEX idx_messages_root ON messages(root_message_id) WHERE root_message_id != '';
CREATE INDEX idx_messages_date ON messages(account_id, date_ms DESC);
CREATE TABLE message_bodies (
	id           INTEGER PRIMARY KEY,
	account_id   TEXT NOT NULL,
	folder_scope TEXT NOT NULL DEFAULT '',
	provider_id  TEXT NOT NULL,
	message_id   TEXT NOT NULL DEFAULT '',
	subject      TEXT NOT NULL DEFAULT '',
	from_name    TEXT NOT NULL DEFAULT '',
	from_email   TEXT NOT NULL DEFAULT '',
	date_ms      INTEGER NOT NULL DEFAULT 0,
	body_text    TEXT NOT NULL DEFAULT '',
	body_html    TEXT NOT NULL DEFAULT '',
	search_text  TEXT NOT NULL DEFAULT '',
	fetched_ms   INTEGER NOT NULL,
	UNIQUE (account_id, folder_scope, provider_id)
);
CREATE VIRTUAL TABLE message_bodies_fts USING fts5(
	subject, from_name, from_email, search_text,
	content='message_bodies', content_rowid='id',
	tokenize='porter unicode61'
)
/* message_bodies_fts(subject,from_name,from_email,search_text) */;
CREATE TABLE IF NOT EXISTS 'message_bodies_fts_data'(id INTEGER PRIMARY KEY, block BLOB);
CREATE TABLE IF NOT EXISTS 'message_bodies_fts_idx'(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS 'message_bodies_fts_docsize'(id INTEGER PRIMARY KEY, sz BLOB);
CREATE TABLE IF NOT EXISTS 'message_bodies_fts_config'(k PRIMARY KEY, v) WITHOUT ROWID;
CREATE TRIGGER message_bodies_fts_insert AFTER INSERT ON message_bodies BEGIN
	INSERT INTO message_bodies_fts(rowid, subject, from_name, from_email, search_text)
	VALUES (new.id, new.subject, new.from_name, new.from_email, new.search_text);
END;
CREATE TRIGGER message_bodies_fts_delete AFTER DELETE ON message_bodies BEGIN
	INSERT INTO message_bodies_fts(message_bodies_fts, rowid, subject, from_name, from_email, search_text)
	VALUES ('delete', old.id, old.subject, old.from_name, old.from_email, old.search_text);
END;
CREATE TRIGGER message_bodies_fts_update AFTER UPDATE ON message_bodies BEGIN
	INSERT INTO message_bodies_fts(message_bodies_fts, rowid, subject, from_name, from_email, search_text)
	VALUES ('delete', old.id, old.subject, old.from_name, old.from_email, old.search_text);
	INSERT INTO message_bodies_fts(rowid, subject, from_name, from_email, search_text)
	VALUES (new.id, new.subject, new.from_name, new.from_email, new.search_text);
END;
PRAGMA user_version = 2;
