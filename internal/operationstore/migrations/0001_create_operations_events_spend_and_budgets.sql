-- Text the store sorts by is in the "C" collation, byte order, as SQLite
-- compared it, so lists come back in the order they always did.
--
-- Every time is Unix nanoseconds, as the SQLite store kept it, so ordering by
-- created_at and the times inside the JSON agree to the nanosecond.

CREATE TABLE operations (
	id                  text COLLATE "C" PRIMARY KEY,
	type                text NOT NULL,
	organization_id     text NOT NULL,
	principal_id        text NOT NULL,
	idempotency_key     text NOT NULL,
	intent_fingerprint  text NOT NULL,
	parent_operation_id text NOT NULL,
	root_operation_id   text NOT NULL,
	state               text NOT NULL,
	revision            bigint NOT NULL,
	intent_json         text NOT NULL,
	receipt_json        text NOT NULL,
	lease_owner         text NOT NULL DEFAULT '',
	lease_token         bigint NOT NULL DEFAULT 0,
	lease_expires_at    bigint NOT NULL DEFAULT 0,
	created_at          bigint NOT NULL,
	updated_at          bigint NOT NULL
);
CREATE UNIQUE INDEX operations_idempotency
	ON operations (principal_id, organization_id, type, idempotency_key);
CREATE INDEX operations_state_created ON operations (state, created_at);
-- Claim takes the oldest queued operation; this index holds only those, so a
-- claim reads one entry however many operations have finished.
CREATE INDEX operations_queued ON operations (created_at, id) WHERE state = 'queued';
CREATE INDEX operations_parent ON operations (parent_operation_id) WHERE parent_operation_id <> '';
CREATE INDEX operations_organization_created ON operations (organization_id, created_at);

CREATE TABLE operation_events (
	operation_id text NOT NULL REFERENCES operations (id),
	sequence     bigint NOT NULL,
	kind         text NOT NULL,
	event_json   text NOT NULL,
	at           bigint NOT NULL,
	PRIMARY KEY (operation_id, sequence)
);

CREATE TABLE operation_spend (
	operation_id       text NOT NULL REFERENCES operations (id),
	root_operation_id  text NOT NULL,
	organization_id    text NOT NULL,
	model              text NOT NULL,
	input_tokens       bigint NOT NULL,
	output_tokens      bigint NOT NULL,
	cache_read_tokens  bigint NOT NULL,
	cache_write_tokens bigint NOT NULL,
	cost_usd           double precision NOT NULL,
	cost_known         boolean NOT NULL,
	at                 bigint NOT NULL
);
CREATE INDEX operation_spend_organization_at ON operation_spend (organization_id, at);
CREATE INDEX operation_spend_root ON operation_spend (root_operation_id);
CREATE INDEX operation_spend_operation ON operation_spend (operation_id);

CREATE TABLE organization_budgets (
	organization_id   text COLLATE "C" PRIMARY KEY,
	monthly_limit_usd double precision NOT NULL,
	updated_by        text NOT NULL,
	updated_at        bigint NOT NULL
);
