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
// It stores METADATA ONLY and never bodies. mailstack stays a live proxy for
// content, so an indexed subject going slightly stale is cosmetic, while a
// cached body would be both a correctness problem and a storage one.
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
