-- Text the store sorts by is in the "C" collation, byte order, as SQLite
-- compared it, so lists come back in the order they always did.
CREATE TABLE hooks (
	id         text COLLATE "C" PRIMARY KEY,
	harness    text NOT NULL,
	event      text NOT NULL,
	matcher    text NOT NULL DEFAULT '',
	command    text NOT NULL,
	scope_kind text NOT NULL,
	scope_id   text NOT NULL DEFAULT '',
	enabled    boolean NOT NULL DEFAULT true,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	CHECK (scope_kind IN ('global', 'instance', 'session')),
	CHECK (scope_kind = 'global' OR scope_id <> '')
);
CREATE INDEX hooks_harness_event ON hooks (harness, event);
CREATE INDEX hooks_scope ON hooks (scope_kind, scope_id);
