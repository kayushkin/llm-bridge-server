-- One row per file per phase of a tool call. The file's contents are a blob in
-- the bare git repository the store keeps beside the database
-- (snapshot_store.git_path); blob_sha names it.
CREATE TABLE snapshots (
	session_id  text NOT NULL,
	tool_use_id text NOT NULL,
	phase       text NOT NULL,
	file_path   text NOT NULL,
	blob_sha    text NOT NULL DEFAULT '',
	size        bigint NOT NULL DEFAULT 0,
	is_binary   boolean NOT NULL DEFAULT false,
	too_large   boolean NOT NULL DEFAULT false,
	missing     boolean NOT NULL DEFAULT false,
	created_at  timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (session_id, tool_use_id, phase, file_path),
	CHECK (phase IN ('before', 'after'))
);
CREATE INDEX snapshots_created ON snapshots (created_at);
CREATE INDEX snapshots_blob_sha ON snapshots (blob_sha);
