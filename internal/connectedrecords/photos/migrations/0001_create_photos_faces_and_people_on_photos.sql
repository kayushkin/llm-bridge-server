-- photo-store: what is known about one owner's photos and videos, the faces a
-- model found on them, who is in them, and groups of faces waiting to be
-- named. Times are epoch seconds, 0 meaning unset, as the API has always
-- served them.
--
-- A record's public id is its type's prefix and its number (FormatRecordID):
-- photo_000001, face_000001, photo_person_000001, face_group_000001,
-- face_group_guess_000001. Only the number is stored.
--
-- Every row carries owner_principal_id, principal-store's id of the person who
-- keeps it. A child row carries its parent's owner, and its foreign key runs
-- over (owner_principal_id, parent), so a child can never hang on another
-- owner's record: a face of one owner's photo can never sit in another owner's
-- group, nor name another owner's link. Every uniqueness rule is per owner:
-- two people may each keep a photo of the same original file.

-- One photo or video. The original stays where it was found (source_machine,
-- source_path); its thumbnail's bytes are a file in file-store, named by
-- thumbnail_file_id, which is '' only once the photo is purged. Nothing is
-- hard-deleted: DELETE stamps deleted_at, and every update, delete and restore
-- first copies the whole row into photo_revisions.
CREATE TABLE photos (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    -- of the original file; what an import finds the owner's photo again by
    sha256             text   NOT NULL,
    source_machine     text   NOT NULL,
    source_path        text   NOT NULL,
    folder             text   NOT NULL DEFAULT '',
    media_type         text   NOT NULL,
    taken_at           bigint NOT NULL DEFAULT 0,
    date_source        text   NOT NULL,
    latitude           double precision,
    longitude          double precision,
    width              bigint NOT NULL DEFAULT 0,
    height             bigint NOT NULL DEFAULT 0,
    -- markdown; [text](ref:<id>) links name people and events
    description        text   NOT NULL DEFAULT '',
    described_by       text   NOT NULL DEFAULT '',
    tags               jsonb  NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags) = 'array'),
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    deleted_at         bigint NOT NULL DEFAULT 0,
    -- when its thumbnail and description were removed for good; 0 = never
    purged_at          bigint NOT NULL DEFAULT 0,
    thumbnail_file_id  text   NOT NULL DEFAULT '',
    CHECK (thumbnail_file_id <> '' OR purged_at <> 0),
    UNIQUE (owner_principal_id, sha256),
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX photos_taken_index  ON photos (owner_principal_id, deleted_at, taken_at, seq);
CREATE INDEX photos_folder_index ON photos (owner_principal_id, folder, deleted_at, taken_at);
CREATE INDEX photos_tags_index   ON photos USING gin (tags jsonb_path_ops);

-- The whole photo as the API answered it just before a change.
CREATE TABLE photo_revisions (
    revision_id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL,
    photo_seq          bigint NOT NULL,
    -- update | import | delete | restore
    reason             text   NOT NULL,
    replaced_at        bigint NOT NULL,
    row_json           jsonb  NOT NULL,
    FOREIGN KEY (owner_principal_id, photo_seq) REFERENCES photos (owner_principal_id, seq)
);
CREATE INDEX photo_revisions_photo_index ON photo_revisions (photo_seq, revision_id DESC);

-- Every place a photo was found: one picture can be sent in several chat
-- messages, or sit in several folders, and the photo row names only the
-- latest. An import adds the place it found the photo and never removes one.
CREATE TABLE photo_sightings (
    owner_principal_id text   NOT NULL,
    photo_seq          bigint NOT NULL,
    source_machine     text   NOT NULL,
    source_path        text   NOT NULL,
    folder             text   NOT NULL,
    -- when an import first found it there
    seen_at            bigint NOT NULL,
    PRIMARY KEY (photo_seq, source_machine, source_path),
    FOREIGN KEY (owner_principal_id, photo_seq) REFERENCES photos (owner_principal_id, seq)
);
CREATE INDEX photo_sightings_place_index ON photo_sightings (owner_principal_id, source_machine, source_path);

-- A photo is scanned once per model; a scan is kept even when it found no
-- face, so a scanner knows what is left.
CREATE TABLE face_scans (
    owner_principal_id text   NOT NULL,
    photo_seq          bigint NOT NULL,
    model              text   NOT NULL,
    face_count         bigint NOT NULL,
    scanned_at         bigint NOT NULL,
    PRIMARY KEY (photo_seq, model),
    FOREIGN KEY (owner_principal_id, photo_seq) REFERENCES photos (owner_principal_id, seq)
);

-- A face a model found on a photo's thumbnail. The box is in fractions of the
-- thumbnail's width and height, and may run past its edges a little. The
-- embedding is little-endian float32s normalized to length 1. A face is
-- retired, never deleted, when the picture it was found on changes.
CREATE TABLE faces (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL,
    photo_seq          bigint NOT NULL,
    model              text   NOT NULL,
    box_left           double precision NOT NULL,
    box_top            double precision NOT NULL,
    box_right          double precision NOT NULL,
    box_bottom         double precision NOT NULL,
    detection_score    double precision NOT NULL,
    embedding          bytea  NOT NULL,
    created_at         bigint NOT NULL,
    retired_at         bigint NOT NULL DEFAULT 0,
    retired_reason     text   NOT NULL DEFAULT '',
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, photo_seq) REFERENCES photos (owner_principal_id, seq)
);
CREATE INDEX faces_photo_index ON faces (photo_seq, model);
CREATE INDEX faces_owner_index ON faces (owner_principal_id, seq);

-- Who is in a photo: a person in people-store (person_000001), by id, never by
-- name. A link is suggested, confirmed, or rejected, which no matcher may
-- undo. face_seq is the face the person is, when known. Every change first
-- copies the row to photo_person_revisions.
CREATE TABLE photo_people (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL,
    photo_seq          bigint NOT NULL,
    person_id          text   NOT NULL,
    face_seq           bigint,
    status             text   NOT NULL CHECK (status IN ('suggested', 'confirmed', 'rejected')),
    confidence         double precision NOT NULL DEFAULT 0,
    evidence           text   NOT NULL DEFAULT '',
    proposed_by        text   NOT NULL,
    decided_by         text   NOT NULL DEFAULT '',
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    UNIQUE (owner_principal_id, photo_seq, person_id),
    UNIQUE (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, photo_seq) REFERENCES photos (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, face_seq) REFERENCES faces (owner_principal_id, seq)
);
CREATE INDEX photo_people_person_index ON photo_people (owner_principal_id, person_id, status);
CREATE INDEX photo_people_face_index   ON photo_people (face_seq) WHERE face_seq IS NOT NULL;

-- The whole link as the API answered it just before a change.
CREATE TABLE photo_person_revisions (
    revision_id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL,
    photo_person_seq   bigint NOT NULL,
    replaced_at        bigint NOT NULL,
    row_json           jsonb  NOT NULL,
    FOREIGN KEY (owner_principal_id, photo_person_seq) REFERENCES photo_people (owner_principal_id, seq)
);

-- Faces a grouper found to be one person, waiting for a person to say who
-- (open), then named as a people-store person, marked as someone not to name
-- (not_known), or left as more than one person (mixed). A face is in at most
-- one group at a time; a face taken out keeps its row with released_at set.
CREATE TABLE face_groups (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL CHECK (owner_principal_id ~ '^principal_[0-9]{6,}$'),
    model              text   NOT NULL,
    status             text   NOT NULL CHECK (status IN ('open', 'named', 'not_known', 'mixed')),
    person_id          text   NOT NULL DEFAULT '',
    made_by            text   NOT NULL,
    decided_by         text   NOT NULL DEFAULT '',
    decided_at         bigint NOT NULL DEFAULT 0,
    created_at         bigint NOT NULL,
    updated_at         bigint NOT NULL,
    UNIQUE (owner_principal_id, seq)
);
CREATE INDEX face_groups_owner_index ON face_groups (owner_principal_id, status, seq);

-- One owner column for both foreign keys: a group and its faces always have
-- one owner.
CREATE TABLE face_group_members (
    owner_principal_id text   NOT NULL,
    group_seq          bigint NOT NULL,
    face_seq           bigint NOT NULL,
    similarity         double precision NOT NULL,
    added_at           bigint NOT NULL,
    released_at        bigint NOT NULL DEFAULT 0,
    released_by        text   NOT NULL DEFAULT '',
    PRIMARY KEY (group_seq, face_seq),
    FOREIGN KEY (owner_principal_id, group_seq) REFERENCES face_groups (owner_principal_id, seq),
    FOREIGN KEY (owner_principal_id, face_seq) REFERENCES faces (owner_principal_id, seq)
);
CREATE UNIQUE INDEX face_group_members_face_index ON face_group_members (face_seq) WHERE released_at = 0;

-- Who a group might be, from a model or the context of its photos. Kept as
-- written; a later guess is added beside it.
CREATE TABLE face_group_guesses (
    seq                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_principal_id text   NOT NULL,
    group_seq          bigint NOT NULL,
    person_id          text   NOT NULL DEFAULT '',
    confidence         double precision NOT NULL,
    evidence           text   NOT NULL DEFAULT '',
    proposed_by        text   NOT NULL,
    created_at         bigint NOT NULL,
    FOREIGN KEY (owner_principal_id, group_seq) REFERENCES face_groups (owner_principal_id, seq)
);
CREATE INDEX face_group_guesses_group_index ON face_group_guesses (group_seq, seq DESC);
