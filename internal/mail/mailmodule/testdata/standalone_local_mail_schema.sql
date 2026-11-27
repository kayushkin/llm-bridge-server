CREATE TABLE local_messages (
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
	unread         INTEGER NOT NULL DEFAULT 1,
	flagged        INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL, text_bytes INTEGER, html_bytes INTEGER, bcc_addrs           TEXT NOT NULL DEFAULT '[]', provider_message_id TEXT NOT NULL DEFAULT '', source_revision     TEXT NOT NULL DEFAULT '', outlook_store_id    TEXT NOT NULL DEFAULT '', outlook_folder_id   TEXT NOT NULL DEFAULT '', outlook_folder_path TEXT NOT NULL DEFAULT '', revision            INTEGER NOT NULL DEFAULT 1, content_sha256      TEXT NOT NULL DEFAULT '', updated_at          INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, id)
);
CREATE INDEX idx_local_messages_listing
	ON local_messages (account_id, folder_id, date_ms DESC, id DESC);
CREATE INDEX idx_local_messages_message_id
	ON local_messages (account_id, message_id);
CREATE TABLE local_message_bodies (
	account_id TEXT NOT NULL,
	id         TEXT NOT NULL,
	body_text  TEXT,
	body_html  TEXT,
	PRIMARY KEY (account_id, id)
);
CREATE UNIQUE INDEX idx_local_messages_provider_message_id
	ON local_messages (account_id, provider_message_id) WHERE provider_message_id <> '';
CREATE TABLE local_import_sources (
	account_id    TEXT NOT NULL,
	source_ref    TEXT NOT NULL,
	source_sha256 TEXT NOT NULL,
	message_count INTEGER NOT NULL,
	imported_at   INTEGER NOT NULL,
	PRIMARY KEY (account_id, source_ref)
);
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
CREATE TABLE ingress_credentials (
	credential_id TEXT PRIMARY KEY,
	token_sha256  TEXT NOT NULL UNIQUE,
	source        TEXT NOT NULL,
	note          TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	revoked_at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE ingress_credential_accounts (
	credential_id TEXT NOT NULL,
	account_id    TEXT NOT NULL,
	PRIMARY KEY (credential_id, account_id)
);
CREATE TABLE delivery_receipts (
	receipt_id          INTEGER PRIMARY KEY AUTOINCREMENT,
	credential_id       TEXT NOT NULL,
	source              TEXT NOT NULL,
	delivery_id         TEXT NOT NULL,
	account_id          TEXT NOT NULL,
	provider_message_id TEXT NOT NULL,
	source_revision     TEXT NOT NULL,
	content_sha256      TEXT NOT NULL,
	status              TEXT NOT NULL,
	message_id          TEXT NOT NULL DEFAULT '',
	revision            INTEGER NOT NULL DEFAULT 0,
	detail              TEXT NOT NULL DEFAULT '',
	received_at         INTEGER NOT NULL,
	UNIQUE (source, delivery_id)
);
CREATE TABLE message_purges (
	account_id          TEXT NOT NULL,
	provider_message_id TEXT NOT NULL,
	message_id          TEXT NOT NULL,
	reason              TEXT NOT NULL,
	purged_at           INTEGER NOT NULL,
	PRIMARY KEY (account_id, provider_message_id)
);
CREATE TABLE message_events (
	seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
	kind                TEXT NOT NULL,
	origin              TEXT NOT NULL,
	account_id          TEXT NOT NULL,
	id                  TEXT NOT NULL,
	provider_message_id TEXT NOT NULL DEFAULT '',
	revision            INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL
);
CREATE INDEX idx_message_events_account ON message_events (account_id, seq);
PRAGMA user_version = 4;
