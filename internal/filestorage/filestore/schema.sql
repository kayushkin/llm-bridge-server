-- file-store schema. Create-only and idempotent: Open() executes this whole
-- file on every boot. A new column goes in ensureColumns in store.go, never in
-- an edit to a CREATE TABLE below — an edited CREATE TABLE is a no-op against a
-- database that already exists.

-- A file is one upload: these bytes, under this name, put here by this service
-- for this thing of its own. The bytes are not in this table. They are on disk,
-- named by their SHA-256, so the same bytes uploaded twice are stored once and
-- two rows point at them. A row is what has an owner and a lifetime; a blob
-- lives exactly as long as some row, deleted or not, still names it.
--
-- owner_service and owner_ref are not foreign keys: the thing a file is
-- attached to belongs to another database. They say who put the file here and
-- for what ("kanban-store", a card id), which is what lets that service list
-- its own files and what a retention sweep will ask.
CREATE TABLE IF NOT EXISTS files (
    id                       TEXT PRIMARY KEY,          -- file_000001; see formatID
    seq                      INTEGER NOT NULL UNIQUE,   -- monotonic; generates id
    sha256                   TEXT NOT NULL,             -- hex; names the blob on disk
    size_bytes               INTEGER NOT NULL,
    content_type             TEXT NOT NULL,             -- what the uploader declared
    detected_content_type    TEXT NOT NULL,             -- what the first bytes look like
    filename                 TEXT NOT NULL,             -- a name, never a path
    owner_service            TEXT NOT NULL,
    owner_ref                TEXT NOT NULL,
    uploaded_by_principal_id TEXT NOT NULL DEFAULT '',  -- principal-store id; the caller vouches for it
    created_at               INTEGER NOT NULL,          -- unix seconds
    deleted_at               INTEGER NOT NULL DEFAULT 0 -- 0 = live; DELETE is reversible
);
CREATE INDEX IF NOT EXISTS idx_files_owner  ON files(owner_service, owner_ref);
CREATE INDEX IF NOT EXISTS idx_files_sha256 ON files(sha256);

-- The behavior settings this service stores for itself (servicesettings).
CREATE TABLE IF NOT EXISTS service_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Where the next id comes from. NOT MAX(seq) + 1 over files: a purge removes
-- the row, and the next upload would then be handed the purged file's id — so a
-- reference to the old file, left anywhere, would resolve to somebody else's.
-- An id is given out once. The counter starts from whatever the table holds,
-- which is right for a database made before this existed.
CREATE TABLE IF NOT EXISTS file_sequence (
    only_row INTEGER PRIMARY KEY CHECK (only_row = 1),
    last_seq INTEGER NOT NULL
);
INSERT OR IGNORE INTO file_sequence (only_row, last_seq)
    VALUES (1, (SELECT COALESCE(MAX(seq), 0) FROM files));
