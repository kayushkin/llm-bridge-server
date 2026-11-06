-- possession-store schema. DRAFT for review: no service reads it yet.
--
-- Create-only and idempotent: Open() will run this whole file on every boot.
-- A new column goes in an additive migration, never in an edit to a CREATE
-- TABLE below.
--
-- Ids are minted here as <prefix>_000001, one prefix per kind of record, so a
-- chat chip can name and draw any of them. Timestamps are epoch seconds.
-- Money is integer cents with a currency code. Nothing a person can cite is
-- hard-deleted: DELETE stamps deleted_at, and every change first copies the
-- whole row into revisions.

PRAGMA foreign_keys = ON;

-- ---------------------------------------------------------------------------
-- Possessions: what every owned thing has, whatever kind it is.
--
-- A kind (garment, computer...) is a table of its own holding only that
-- kind's facts, one row per possession, with its own id prefix. A possession
-- with no kind row is still a full record: a lamp needs no lamp table. A new
-- kind table is added when enough possessions share facts worth querying.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS possessions (
    id                    TEXT PRIMARY KEY,          -- possession_000001
    seq                   INTEGER NOT NULL UNIQUE,
    name                  TEXT NOT NULL,             -- what the owner calls it: "black Carhartt hoodie"
    brand                 TEXT NOT NULL DEFAULT '',
    model                 TEXT NOT NULL DEFAULT '',
    serial_number         TEXT NOT NULL DEFAULT '',
    description           TEXT NOT NULL DEFAULT '',  -- markdown
    quantity              INTEGER NOT NULL DEFAULT 1, -- 6 identical white socks are one possession with quantity 6
    condition             TEXT NOT NULL DEFAULT '',  -- vocabulary: new, good, worn, damaged, broken; '' = not known
    status                TEXT NOT NULL,             -- vocabulary: owned, lent_out, sold, given_away, lost, discarded
    place_id              TEXT,                      -- location-store place_…; NULL when not known
    spot                  TEXT NOT NULL DEFAULT '',  -- where inside the place: "bedroom closet, top shelf"
    acquired_at           INTEGER NOT NULL DEFAULT 0, -- 0 = not known
    acquired_how          TEXT NOT NULL DEFAULT '',  -- vocabulary: bought, gift, made, inherited, found; '' = not known
    purchase_line_id      TEXT REFERENCES purchase_lines(id), -- the order line it came from, when one is known
    price_cents           INTEGER,                   -- what was paid, when no purchase line says it; NULL = not known
    price_currency        TEXT NOT NULL DEFAULT '',  -- ISO 4217, required with price_cents
    disposed_at           INTEGER NOT NULL DEFAULT 0, -- when it stopped being owned; 0 while owned
    tags                  TEXT NOT NULL DEFAULT '[]',
    created_by            TEXT NOT NULL,             -- principal_… or the review that made it (photo_review_…)
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    deleted_at            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_possessions_status ON possessions(deleted_at, status);
CREATE INDEX IF NOT EXISTS idx_possessions_place ON possessions(place_id);

-- Which photo-store photos show a possession, and where in each. The box lets
-- a chip draw the possession cut from a photo of the whole room.
CREATE TABLE IF NOT EXISTS possession_photos (
    possession_id         TEXT NOT NULL REFERENCES possessions(id),
    photo_id              TEXT NOT NULL,             -- photo-store photo_…
    box_left              REAL,                      -- fractions of the photo, 0..1; all four or none
    box_top               REAL,
    box_right             REAL,
    box_bottom            REAL,
    is_cover              INTEGER NOT NULL DEFAULT 0, -- the one picture a chip shows; at most one per possession
    sighting_id           TEXT REFERENCES sightings(id), -- the review finding that linked it; NULL when linked by hand
    linked_by             TEXT NOT NULL,
    linked_at             INTEGER NOT NULL,
    PRIMARY KEY (possession_id, photo_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_possession_photos_cover ON possession_photos(possession_id) WHERE is_cover = 1;
CREATE INDEX IF NOT EXISTS idx_possession_photos_photo ON possession_photos(photo_id);

-- ---------------------------------------------------------------------------
-- Kinds. Each row extends one possession.
-- ---------------------------------------------------------------------------

-- Clothes, shoes and worn accessories: what outfits are made of.
CREATE TABLE IF NOT EXISTS garments (
    id                    TEXT PRIMARY KEY,          -- garment_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    category              TEXT NOT NULL,             -- vocabulary: top, bottom, one_piece, outerwear, footwear, headwear, accessory, underwear, sleepwear, swimwear
    garment_type          TEXT NOT NULL DEFAULT '',  -- vocabulary per category: t_shirt, hoodie, jeans, chinos, sneakers, boots...
    layer                 TEXT NOT NULL DEFAULT '',  -- vocabulary: base, mid, outer; '' for footwear and accessories
    primary_color         TEXT NOT NULL DEFAULT '',  -- vocabulary of named colors, so outfit rules can match them
    secondary_colors      TEXT NOT NULL DEFAULT '[]',
    pattern               TEXT NOT NULL DEFAULT '',  -- vocabulary: solid, striped, plaid, graphic, floral, camo...
    materials             TEXT NOT NULL DEFAULT '[]', -- ["cotton","polyester"]
    size_label            TEXT NOT NULL DEFAULT '',  -- as printed: "L", "32x30", "10.5"
    size_system           TEXT NOT NULL DEFAULT '',  -- vocabulary: letter, waist_inseam_inches, us_shoe_men...
    fit                   TEXT NOT NULL DEFAULT '',  -- vocabulary: slim, regular, relaxed, oversized
    formality             INTEGER,                   -- 1 (gym) .. 5 (formal); NULL = not judged
    warmth                INTEGER,                   -- 1 (hot day) .. 5 (snow); NULL = not judged
    seasons               TEXT NOT NULL DEFAULT '[]' -- ["spring","fall"]
);

-- A computer: desktop, laptop, server, phone, tablet, single-board.
-- harness_machine_id ties it to the machine record agents run on.
CREATE TABLE IF NOT EXISTS computers (
    id                    TEXT PRIMARY KEY,          -- computer_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    form_factor           TEXT NOT NULL,             -- vocabulary: desktop, laptop, server, phone, tablet, single_board, console
    harness_machine_id    TEXT UNIQUE,               -- harness-store machines.id (m_…); NULL when agents do not run on it
    processor             TEXT NOT NULL DEFAULT '',
    memory_gigabytes      INTEGER,
    graphics              TEXT NOT NULL DEFAULT '',
    operating_system      TEXT NOT NULL DEFAULT ''
);

-- Parts that live inside or plug into a computer and are worth tracking on
-- their own: drives, graphics cards, monitors, keyboards.
CREATE TABLE IF NOT EXISTS computer_parts (
    id                    TEXT PRIMARY KEY,          -- computer_part_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    part_type             TEXT NOT NULL,             -- vocabulary: storage_drive, graphics_card, memory_module, monitor, keyboard, mouse, dock, network_device...
    installed_in_computer_id TEXT REFERENCES computers(id), -- NULL when loose or shared
    capacity_gigabytes    INTEGER                    -- drives and memory; NULL otherwise
);

-- Things that make, carry or play sound.
CREATE TABLE IF NOT EXISTS audio_devices (
    id                    TEXT PRIMARY KEY,          -- audio_device_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    device_type           TEXT NOT NULL,             -- vocabulary: headphones, earbuds, speakers, amplifier, audio_interface, microphone, mixer, effects_pedal, converter, turntable, cable
    connections           TEXT NOT NULL DEFAULT '[]', -- ["xlr","quarter_inch","usb_c","bluetooth"]
    powered               INTEGER                    -- 1 needs power, 0 passive; NULL = not known
);

-- A physical instrument. music_instrument_id is music-practice-store's
-- catalog entry for what kind of instrument it is (electric guitar...).
CREATE TABLE IF NOT EXISTS musical_instruments (
    id                    TEXT PRIMARY KEY,          -- musical_instrument_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    music_instrument_id   TEXT,                      -- music-practice-store music_instrument_…
    string_count          INTEGER,                   -- NULL for unstrung instruments
    finish                TEXT NOT NULL DEFAULT '',
    current_string_set    TEXT NOT NULL DEFAULT '',  -- "Ernie Ball Regular Slinky 10-46"
    strings_changed_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS vehicles (
    id                    TEXT PRIMARY KEY,          -- vehicle_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    vehicle_type          TEXT NOT NULL,             -- vocabulary: car, truck, motorcycle, bicycle, scooter
    model_year            INTEGER,
    vehicle_identification_number TEXT NOT NULL DEFAULT '',
    license_plate         TEXT NOT NULL DEFAULT '',
    registration_state    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS vehicle_odometer_readings (
    vehicle_id            TEXT NOT NULL REFERENCES vehicles(id),
    read_at               INTEGER NOT NULL,
    miles                 INTEGER NOT NULL,
    source                TEXT NOT NULL,             -- principal_…, a photo_…, a service receipt
    PRIMARY KEY (vehicle_id, read_at)
);

-- ---------------------------------------------------------------------------
-- Outfits.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS outfits (
    id                    TEXT PRIMARY KEY,          -- outfit_000001
    seq                   INTEGER NOT NULL UNIQUE,
    name                  TEXT NOT NULL DEFAULT '',
    occasion              TEXT NOT NULL DEFAULT '',  -- "date night", "office", "hiking"
    weather               TEXT NOT NULL DEFAULT '',  -- what it was made for: "45F, rain"
    status                TEXT NOT NULL,             -- vocabulary: suggested, kept, rejected
    reasoning             TEXT NOT NULL DEFAULT '',  -- why a generator put these together
    preview_image_path    TEXT NOT NULL DEFAULT '',  -- a drawn picture of it, when one was made
    created_by            TEXT NOT NULL,             -- principal_…, or the model id that suggested it
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    deleted_at            INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS outfit_garments (
    outfit_id             TEXT NOT NULL REFERENCES outfits(id),
    garment_id            TEXT NOT NULL REFERENCES garments(id),
    position              INTEGER NOT NULL,          -- order to draw them, inner to outer, head to foot
    PRIMARY KEY (outfit_id, garment_id)
);

-- Days an outfit was worn, so a generator can avoid repeats and learn favorites.
CREATE TABLE IF NOT EXISTS outfit_wearings (
    outfit_id             TEXT NOT NULL REFERENCES outfits(id),
    worn_on               TEXT NOT NULL,             -- YYYY-MM-DD in the owner's time zone
    PRIMARY KEY (outfit_id, worn_on)
);

-- ---------------------------------------------------------------------------
-- Purchases: orders from Amazon, Facebook Marketplace, a store receipt.
-- A line becomes a possession once someone confirms it is one.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS purchases (
    id                    TEXT PRIMARY KEY,          -- possession_purchase_000001
    seq                   INTEGER NOT NULL UNIQUE,
    seller                TEXT NOT NULL DEFAULT '',
    marketplace           TEXT NOT NULL,             -- vocabulary: amazon, facebook_marketplace, ebay, in_store, other
    order_reference       TEXT NOT NULL DEFAULT '',  -- the marketplace's own order number; an import finds the order again by it
    ordered_at            INTEGER NOT NULL,
    total_cents           INTEGER,
    currency              TEXT NOT NULL DEFAULT '',
    source                TEXT NOT NULL,             -- vocabulary: amazon_export, facebook_export, email_receipt, manual
    created_at            INTEGER NOT NULL,
    deleted_at            INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_purchases_order ON purchases(marketplace, order_reference) WHERE order_reference != '';

CREATE TABLE IF NOT EXISTS purchase_lines (
    id                    TEXT PRIMARY KEY,          -- purchase_line_000001
    purchase_id           TEXT NOT NULL REFERENCES purchases(id),
    line_number           INTEGER NOT NULL,
    title                 TEXT NOT NULL,             -- as the seller wrote it
    quantity              INTEGER NOT NULL DEFAULT 1,
    unit_price_cents      INTEGER,
    product_reference     TEXT NOT NULL DEFAULT '',  -- ASIN, listing id
    disposition           TEXT NOT NULL DEFAULT 'unreviewed', -- vocabulary: unreviewed, possession, consumable, returned, gift_for_someone_else
    UNIQUE (purchase_id, line_number)
);

-- ---------------------------------------------------------------------------
-- Photo review: two models read each photo on their own, then settle on one
-- list together; what they cannot settle becomes a question for the owner.
-- ---------------------------------------------------------------------------

-- One photo going through review.
CREATE TABLE IF NOT EXISTS photo_reviews (
    id                    TEXT PRIMARY KEY,          -- photo_review_000001
    seq                   INTEGER NOT NULL UNIQUE,
    photo_id              TEXT NOT NULL UNIQUE,      -- photo-store photo_…
    status                TEXT NOT NULL,             -- vocabulary: queued, reading, reconciling, waiting_for_owner, done, failed
    error                 TEXT NOT NULL DEFAULT '',  -- why it failed, in full
    queued_by             TEXT NOT NULL,
    queued_at             INTEGER NOT NULL,
    finished_at           INTEGER NOT NULL DEFAULT 0
);

-- What one model said in one round. Round 1 is each model alone; round 2
-- each model sees the other's round-1 answer and the existing possessions
-- it may be looking at again.
CREATE TABLE IF NOT EXISTS model_readings (
    photo_review_id       TEXT NOT NULL REFERENCES photo_reviews(id),
    round                 INTEGER NOT NULL,
    model_id              TEXT NOT NULL,             -- model-store id: claude-opus-5-5, gpt-6-astra
    answer                TEXT NOT NULL,             -- the model's structured answer as JSON, unchanged
    input_tokens          INTEGER NOT NULL,
    output_tokens         INTEGER NOT NULL,
    cost_usd              REAL NOT NULL,             -- at model-store list price
    read_at               INTEGER NOT NULL,
    PRIMARY KEY (photo_review_id, round, model_id)
);

-- One thing the review found in a photo.
CREATE TABLE IF NOT EXISTS sightings (
    id                    TEXT PRIMARY KEY,          -- sighting_000001
    seq                   INTEGER NOT NULL UNIQUE,
    photo_review_id       TEXT NOT NULL REFERENCES photo_reviews(id),
    box_left              REAL NOT NULL,
    box_top               REAL NOT NULL,
    box_right             REAL NOT NULL,
    box_bottom            REAL NOT NULL,
    proposed              TEXT NOT NULL,             -- the agreed possession and kind fields, as JSON
    agreement             TEXT NOT NULL,             -- vocabulary: both_agree, models_differ, one_model_only
    status                TEXT NOT NULL,             -- vocabulary: proposed, accepted, rejected
    possession_id         TEXT REFERENCES possessions(id), -- set when accepted: a new possession or one already known
    decided_by            TEXT NOT NULL DEFAULT '',  -- principal_… or "agreement" when both models agreed and nothing was asked
    decided_at            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sightings_review ON sightings(photo_review_id);

-- A question for the owner: "Is this the same hoodie as possession_000012?",
-- "Opus says Fender, Astra says Squier: which is it?"
CREATE TABLE IF NOT EXISTS owner_questions (
    id                    TEXT PRIMARY KEY,          -- possession_question_000001
    seq                   INTEGER NOT NULL UNIQUE,
    sighting_id           TEXT REFERENCES sightings(id),
    photo_review_id       TEXT REFERENCES photo_reviews(id),
    question              TEXT NOT NULL,
    choices               TEXT NOT NULL DEFAULT '[]', -- suggested answers; the owner may write another
    answer                TEXT NOT NULL DEFAULT '',
    answered_by           TEXT NOT NULL DEFAULT '',
    asked_at              INTEGER NOT NULL,
    answered_at           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_owner_questions_open ON owner_questions(answered_at, asked_at);

-- ---------------------------------------------------------------------------
-- History.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS revisions (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    record_id             TEXT NOT NULL,             -- any id above
    record                TEXT NOT NULL,             -- the whole row as JSON, before the change
    changed_by            TEXT NOT NULL,
    changed_at            INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_revisions_record ON revisions(record_id, id);
