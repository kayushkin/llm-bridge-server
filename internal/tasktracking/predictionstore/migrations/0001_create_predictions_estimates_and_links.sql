-- The calibration ledger: a claim and the odds on it, every revision of those
-- odds, and what the claim is about. Times are Unix seconds, 0 meaning unset,
-- as the API has always served them.

-- A prediction's public id is prediction_ and its number (FormatRecordID), so
-- the number is the only id stored. `probability` is the newest estimate,
-- copied here; `initial_probability` is the first and never moves, so the gap
-- between them measures how well the author updates.
CREATE TABLE predictions (
    seq                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    claim               text NOT NULL,
    resolution_criteria text NOT NULL,
    category            text NOT NULL,
    tags                jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(tags) = 'array'),
    provenance          text NOT NULL,
    author              text NOT NULL DEFAULT '',
    probability         double precision NOT NULL,
    initial_probability double precision NOT NULL,
    due_at              bigint NOT NULL DEFAULT 0,
    status              text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'void')),
    outcome             text NOT NULL DEFAULT '' CHECK (outcome IN ('', 'true', 'false')),
    resolved_at         bigint NOT NULL DEFAULT 0,
    resolution_note     text NOT NULL DEFAULT '',
    created_at          bigint NOT NULL,
    updated_at          bigint NOT NULL,
    deleted_at          bigint NOT NULL DEFAULT 0,
    -- What the q filter searches, read as the FTS5 tokenizer the store
    -- searched with before read it: every ASCII punctuation mark separates
    -- words, so deploy.sh is "deploy sh" and prediction_000012 is
    -- "prediction 000012", and the simple configuration neither stems nor
    -- drops common words. Without the translate, Postgres keeps deploy.sh,
    -- tools/deploy.go and bridge.db as one word each, and a search for
    -- deploy misses them. searchQueryOf reads a query the same way.
    search_document     tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', translate(
            claim || ' ' || resolution_criteria || ' ' || tags::text || ' ' || resolution_note,
            '!"#$%&''()*+,-./:;<=>?@[\]^_`{|}~', '                                '))
    ) STORED
);
CREATE INDEX predictions_status_index     ON predictions (status, deleted_at);
CREATE INDEX predictions_due_index        ON predictions (due_at, status);
CREATE INDEX predictions_category_index   ON predictions (category);
CREATE INDEX predictions_provenance_index ON predictions (provenance);
CREATE INDEX predictions_author_index     ON predictions (author);
CREATE INDEX predictions_created_index    ON predictions (created_at);
CREATE INDEX predictions_resolved_index   ON predictions (resolved_at);
CREATE INDEX predictions_tags_index       ON predictions USING gin (tags jsonb_path_ops);
CREATE INDEX predictions_search_index     ON predictions USING gin (search_document);

-- Append-only: the store has no statement that updates or deletes a row here.
-- Rewriting old odds is how a calibration record becomes a flattering one.
CREATE TABLE estimates (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    prediction_seq bigint NOT NULL REFERENCES predictions (seq) ON DELETE CASCADE,
    probability    double precision NOT NULL,
    rationale      text NOT NULL DEFAULT '',
    provenance     text NOT NULL DEFAULT '',
    author         text NOT NULL DEFAULT '',
    created_at     bigint NOT NULL
);
CREATE INDEX estimates_prediction_index ON estimates (prediction_seq, created_at);

-- What the prediction is about, by the id the owning store gave it.
-- entity_type is a type from reference-store's list and is not checked here;
-- label is for display and is never read back to find a row.
CREATE TABLE links (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    prediction_seq bigint NOT NULL REFERENCES predictions (seq) ON DELETE CASCADE,
    entity_type    text NOT NULL,
    entity_ref     text NOT NULL,
    label          text NOT NULL DEFAULT '',
    created_at     bigint NOT NULL,
    UNIQUE (prediction_seq, entity_type, entity_ref)
);
CREATE INDEX links_entity_index ON links (entity_type, entity_ref);
