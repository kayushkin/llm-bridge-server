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
