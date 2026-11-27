-- Every note, todo, ranked list and workspace is one row of items, whatever
-- its type. Times are timestamptz to the microsecond.

-- id is a uuid as text: other stores and kanban cards hold these ids, so they
-- keep the form they were given. parent_id has no foreign key, as it never
-- had one: the store checks it on write, and a child whose parent was purged
-- keeps pointing at it.
--
-- due_at keeps the offset its writer sent beside the instant, because the
-- item gives it back in that offset (2026-08-01T12:00:00-07:00 stays -07:00)
-- and timestamptz keeps only the instant. It is stored to the second, as it
-- always was.
CREATE TABLE items (
    id                     text COLLATE "C" PRIMARY KEY,
    type                   text NOT NULL,
    title                  text NOT NULL,
    body                   text NOT NULL DEFAULT '',
    tags                   jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags) = 'array'),
    priority               integer NOT NULL DEFAULT 0,
    rank                   double precision NOT NULL DEFAULT 0,
    status                 text NOT NULL DEFAULT 'open',
    list_id                text COLLATE "C" NOT NULL DEFAULT '',
    due_at                 timestamptz,
    due_at_offset_seconds  integer,
    parent_id              text COLLATE "C",
    links                  jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(links) = 'array'),
    created_by             text NOT NULL DEFAULT '',
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,
    deleted_at             timestamptz,
    -- The recurrence rule as JSON; NULL when the item has none, so "has a
    -- rule" is what the reminder coordinator's partial index is built on.
    schedule               jsonb,
    held_at                timestamptz,
    hold_reason            text NOT NULL DEFAULT '',
    -- NULL means no ceiling; 0 is a real ceiling: hold before spending anything.
    auto_hold_at_usd       double precision,
    -- What GET /api/search reads, split into words as the FTS5 table the
    -- store searched before split them: every ASCII punctuation mark
    -- separates words, so deploy.sh is "deploy sh", and the simple
    -- configuration neither stems nor drops common words. searchTermsOf
    -- splits a query the same way.
    search_document        tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            title || ' ' || body,
            '!"#$%&''()*+,-./:;<=>?@[\]^_`{|}~', '                                '))
    ) STORED,
    CHECK ((due_at IS NULL) = (due_at_offset_seconds IS NULL))
);
CREATE INDEX items_type_index          ON items (type);
CREATE INDEX items_status_index        ON items (status);
CREATE INDEX items_list_index          ON items (list_id);
CREATE INDEX items_created_by_index    ON items (created_by);
CREATE INDEX items_created_at_index    ON items (created_at);
CREATE INDEX items_parent_index        ON items (parent_id);
CREATE INDEX items_deleted_index       ON items (deleted_at);
CREATE INDEX items_tags_index          ON items USING gin (tags jsonb_path_ops);
CREATE INDEX items_search_index        ON items USING gin (search_document);
CREATE INDEX items_scheduled_index     ON items (created_at) WHERE schedule IS NOT NULL;
CREATE INDEX items_held_index          ON items (id) WHERE held_at IS NOT NULL;
CREATE INDEX items_spend_ceiling_index ON items (id) WHERE auto_hold_at_usd IS NOT NULL;

-- The state of an item before each update, delete, restore, hold, unhold and
-- purge. A revision is never changed or removed, and it outlives a purged
-- item, so it has no foreign key to items.
CREATE TABLE item_revisions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    item_id     text COLLATE "C" NOT NULL,
    title       text NOT NULL,
    body        text NOT NULL DEFAULT '',
    tags        jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags) = 'array'),
    status      text NOT NULL DEFAULT 'open',
    priority    integer NOT NULL DEFAULT 0,
    list_id     text NOT NULL DEFAULT '',
    parent_id   text,
    links       jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(links) = 'array'),
    deleted_at  timestamptz,
    reason      text NOT NULL,
    replaced_at timestamptz NOT NULL
);
CREATE INDEX item_revisions_item_index ON item_revisions (item_id, id DESC);
