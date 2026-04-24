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
`
