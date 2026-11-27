-- A file is one upload: these bytes, under this name, put here by this
-- service for this thing of its own. The bytes are not in this table. They
-- are on disk, under the module's data directory, named by their SHA-256, so
-- the same bytes uploaded twice are stored once and two rows point at them. A
-- row is what has an owner and a lifetime; a blob lives exactly as long as
-- some row, deleted or not, still names it.
--
-- A file's public id is file_ and its number (FormatRecordID), so the number
-- is the only id stored. It comes from the identity column, which hands each
-- number out once: a purged file's id is never given to another upload, so a
-- reference left in another store can never open somebody else's file.
--
-- owner_service and owner_ref are not foreign keys: the thing a file is
-- attached to belongs to another store. They say who put the file here and
-- for what (kanban-store and a card id), which is what lets that store list
-- its own files.
CREATE TABLE files (
    seq                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Hex; names the blob on disk.
    sha256                   text   NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes               bigint NOT NULL CHECK (size_bytes > 0),
    -- What the uploader declared, without parameters.
    content_type             text   NOT NULL,
    -- What the first bytes look like.
    detected_content_type    text   NOT NULL,
    -- A name, never a path.
    filename                 text   NOT NULL,
    owner_service            text   NOT NULL,
    owner_ref                text   NOT NULL,
    -- A principal-store id, or ''; the calling service vouches for it.
    uploaded_by_principal_id text   NOT NULL DEFAULT '',
    created_at               bigint NOT NULL,
    -- 0 for a live file; DELETE is reversible and sets it.
    deleted_at               bigint NOT NULL DEFAULT 0
);
CREATE INDEX files_owner_index  ON files (owner_service, owner_ref);
CREATE INDEX files_sha256_index ON files (sha256);
