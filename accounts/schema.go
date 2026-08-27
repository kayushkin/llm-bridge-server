package accounts

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
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

CREATE TABLE IF NOT EXISTS imap_creds (
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

CREATE TABLE IF NOT EXISTS imap_folder_map (
	account_id TEXT NOT NULL,
	canonical  TEXT NOT NULL,
	folder_id  TEXT NOT NULL,
	PRIMARY KEY (account_id, canonical)
);
`

// messagesSchema is the message index: the metadata mailstack needs to answer
// "what messages exist, what are their ids, and how do they relate", without
// asking the provider.
//
// It stores METADATA ONLY and never bodies. This table originally carried the
// rule "never bodies" for the whole store; since migration 0002 bodies ARE
// recorded, in message_bodies below, but only to back full-text search — the
// read path stays a live proxy, and no stored body is ever served back as the
// message. See the comment on messageBodiesSchema.
//
// The primary key is (account_id, folder_scope, provider_id):
//
//   - provider_id is the provider's own message id — Gmail's message id, an
//     IMAP UID.
//   - folder_scope exists because those two are not the same kind of id. A
//     Gmail id identifies a message across the whole account, so its rows carry
//     folder_scope=”. An IMAP UID is unique only WITHIN one mailbox and is
//     invalidated when UIDVALIDITY changes, so its rows carry the folder. Using
//     a bare (account, id) key would silently collide two different IMAP
//     messages that happen to share a UID in different folders.
//
// message_id (RFC 5322) is indexed but nullable and deliberately NOT the key:
// it is optional per RFC 5322 §3.6.4, and a key that can be empty is not a key.
// It is what dedups one message delivered to two accounts.
const messagesSchema = `
CREATE TABLE IF NOT EXISTS messages (
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
CREATE INDEX IF NOT EXISTS idx_messages_message_id ON messages(message_id) WHERE message_id != '';
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(account_id, thread_id) WHERE thread_id != '';
CREATE INDEX IF NOT EXISTS idx_messages_root ON messages(root_message_id) WHERE root_message_id != '';
CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(account_id, date_ms DESC);
`

// messageBodiesSchema is migration 0002: the message payload record and its
// full-text index.
//
// This deliberately revises the "metadata only, never bodies" rule stated above
// messagesSchema. The email-classifier needs "which stored mail reads like this
// one" answered with BM25 over real text, and that question cannot be answered
// live against a provider. So bodies are now recorded — but only to back
// search. Every read of a message (GET /api/messages/{id}) still goes live to
// the provider; nothing serves a stored body back as the message.
//
// message_bodies carries its own copy of subject/from/date rather than joining
// messages, because the FTS index reads its columns from exactly one content
// table. Both rows are written in the same call from the same fetched message,
// so they cannot disagree about what the provider said at fetch time. messages
// stays the identity/threading index; message_bodies is the record of payloads
// fetched.
//
// search_text is the one derived column: the body rendered as plain text for
// indexing (the text part when it is the real message, the de-tagged HTML part
// when the text part is a "view in browser" stub), capped so a single giant
// message cannot bloat the index. body_text and body_html are verbatim.
//
// The FTS table uses external content against message_bodies, kept true by the
// three triggers. Tokenizer is porter+unicode61: stemmed prose matching, and
// addresses split on @ and dots so "wellsfargo" matches the sender domain.
const messageBodiesSchema = `
CREATE TABLE IF NOT EXISTS message_bodies (
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

CREATE VIRTUAL TABLE IF NOT EXISTS message_bodies_fts USING fts5(
	subject, from_name, from_email, search_text,
	content='message_bodies', content_rowid='id',
	tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS message_bodies_fts_insert AFTER INSERT ON message_bodies BEGIN
	INSERT INTO message_bodies_fts(rowid, subject, from_name, from_email, search_text)
	VALUES (new.id, new.subject, new.from_name, new.from_email, new.search_text);
END;

CREATE TRIGGER IF NOT EXISTS message_bodies_fts_delete AFTER DELETE ON message_bodies BEGIN
	INSERT INTO message_bodies_fts(message_bodies_fts, rowid, subject, from_name, from_email, search_text)
	VALUES ('delete', old.id, old.subject, old.from_name, old.from_email, old.search_text);
END;

CREATE TRIGGER IF NOT EXISTS message_bodies_fts_update AFTER UPDATE ON message_bodies BEGIN
	INSERT INTO message_bodies_fts(message_bodies_fts, rowid, subject, from_name, from_email, search_text)
	VALUES ('delete', old.id, old.subject, old.from_name, old.from_email, old.search_text);
	INSERT INTO message_bodies_fts(rowid, subject, from_name, from_email, search_text)
	VALUES (new.id, new.subject, new.from_name, new.from_email, new.search_text);
END;
`
