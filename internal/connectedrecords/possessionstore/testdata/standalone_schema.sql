-- The standalone possession-store's schema as its last release left a database:
-- schema.sql with every column its migrations added. The import test builds
-- a standalone file from it.
CREATE TABLE possessions (
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
, merged_into_id TEXT REFERENCES possessions(id), room TEXT NOT NULL DEFAULT '', product_id TEXT REFERENCES products(id), budget_transaction_id TEXT);
CREATE INDEX idx_possessions_status ON possessions(deleted_at, status);
CREATE INDEX idx_possessions_place ON possessions(place_id);
CREATE TABLE possession_photos (
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
CREATE UNIQUE INDEX idx_possession_photos_cover ON possession_photos(possession_id) WHERE is_cover = 1;
CREATE INDEX idx_possession_photos_photo ON possession_photos(photo_id);
CREATE TABLE garments (
    id                    TEXT PRIMARY KEY,          -- garment_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    category              TEXT NOT NULL,             -- vocabulary: top, bottom, one_piece, outerwear, footwear, headwear, accessory, underwear, sleepwear, swimwear
    garment_type          TEXT NOT NULL DEFAULT '',  -- free words, lower case: t_shirt, hoodie, jeans, chinos, sneakers, boots...
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
CREATE TABLE computers (
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
CREATE TABLE computer_parts (
    id                    TEXT PRIMARY KEY,          -- computer_part_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    part_type             TEXT NOT NULL,             -- vocabulary: storage_drive, graphics_card, memory_module, monitor, keyboard, mouse, dock, network_device...
    installed_in_computer_id TEXT REFERENCES computers(id), -- NULL when loose or shared
    capacity_gigabytes    INTEGER                    -- drives and memory; NULL otherwise
);
CREATE TABLE audio_devices (
    id                    TEXT PRIMARY KEY,          -- audio_device_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    device_type           TEXT NOT NULL,             -- vocabulary: headphones, earbuds, speakers, amplifier, audio_interface, microphone, mixer, effects_pedal, converter, turntable, cable
    connections           TEXT NOT NULL DEFAULT '[]', -- ["xlr","quarter_inch","usb_c","bluetooth"]
    powered               INTEGER                    -- 1 needs power, 0 passive; NULL = not known
);
CREATE TABLE musical_instruments (
    id                    TEXT PRIMARY KEY,          -- musical_instrument_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    music_instrument_id   TEXT,                      -- music-practice-store music_instrument_…
    string_count          INTEGER,                   -- NULL for unstrung instruments
    finish                TEXT NOT NULL DEFAULT '',
    current_string_set    TEXT NOT NULL DEFAULT '',  -- "Ernie Ball Regular Slinky 10-46"
    strings_changed_at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE vehicles (
    id                    TEXT PRIMARY KEY,          -- vehicle_000001
    seq                   INTEGER NOT NULL UNIQUE,
    possession_id         TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    vehicle_type          TEXT NOT NULL,             -- vocabulary: car, truck, motorcycle, bicycle, scooter
    model_year            INTEGER,
    vehicle_identification_number TEXT NOT NULL DEFAULT '',
    license_plate         TEXT NOT NULL DEFAULT '',
    registration_state    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE vehicle_odometer_readings (
    vehicle_id            TEXT NOT NULL REFERENCES vehicles(id),
    read_at               INTEGER NOT NULL,
    miles                 INTEGER NOT NULL,
    source                TEXT NOT NULL,             -- principal_…, a photo_…, a service receipt
    PRIMARY KEY (vehicle_id, read_at)
);
CREATE TABLE outfits (
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
, merged_into_id TEXT REFERENCES outfits(id), favorite INTEGER NOT NULL DEFAULT 0, tags TEXT NOT NULL DEFAULT '[]');
CREATE TABLE outfit_garments (
    outfit_id             TEXT NOT NULL REFERENCES outfits(id),
    garment_id            TEXT NOT NULL REFERENCES garments(id),
    position              INTEGER NOT NULL,          -- order to draw them, inner to outer, head to foot
    PRIMARY KEY (outfit_id, garment_id)
);
CREATE TABLE outfit_wearings (
    outfit_id             TEXT NOT NULL REFERENCES outfits(id),
    worn_on               TEXT NOT NULL,             -- YYYY-MM-DD in the owner's time zone
    PRIMARY KEY (outfit_id, worn_on)
);
CREATE TABLE purchases (
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
CREATE UNIQUE INDEX idx_purchases_order ON purchases(marketplace, order_reference) WHERE order_reference != '';
CREATE TABLE purchase_lines (
    id                    TEXT PRIMARY KEY,          -- purchase_line_000001
    seq                   INTEGER NOT NULL UNIQUE,
    purchase_id           TEXT NOT NULL REFERENCES purchases(id),
    line_number           INTEGER NOT NULL,
    title                 TEXT NOT NULL,             -- as the seller wrote it
    quantity              INTEGER NOT NULL DEFAULT 1,
    unit_price_cents      INTEGER,
    product_reference     TEXT NOT NULL DEFAULT '',  -- ASIN, listing id
    disposition           TEXT NOT NULL DEFAULT 'unreviewed', -- vocabulary: unreviewed, possession, consumable, returned, gift_for_someone_else
    created_at            INTEGER NOT NULL,
    UNIQUE (purchase_id, line_number)
);
CREATE TABLE photo_reviews (
    id                    TEXT PRIMARY KEY,          -- photo_review_000001
    seq                   INTEGER NOT NULL UNIQUE,
    photo_id              TEXT NOT NULL UNIQUE,      -- photo-store photo_…
    status                TEXT NOT NULL,             -- vocabulary: queued, reading, reconciling, waiting_for_owner, done, failed
    image_source          TEXT NOT NULL DEFAULT '',  -- what the readers were shown: original (longest side 2048) or thumbnail
    error                 TEXT NOT NULL DEFAULT '',  -- why it failed, in full
    queued_by             TEXT NOT NULL,
    queued_at             INTEGER NOT NULL,
    finished_at           INTEGER NOT NULL DEFAULT 0
, reading TEXT NOT NULL DEFAULT 'inventory', wearer_person_id TEXT NOT NULL DEFAULT '', outfit_id TEXT REFERENCES outfits(id), worn_on TEXT NOT NULL DEFAULT '');
CREATE TABLE model_readings (
    photo_review_id       TEXT NOT NULL REFERENCES photo_reviews(id),
    round                 INTEGER NOT NULL,
    model_role            TEXT NOT NULL,             -- the model-store role asked: possession-photo-reader…
    model_id              TEXT NOT NULL,             -- the model that answered: claude-opus-5-5, gpt-6-astra
    answer                TEXT NOT NULL,             -- the model's structured answer as JSON, unchanged
    input_tokens          INTEGER NOT NULL,          -- input, cache read and cache write together
    output_tokens         INTEGER NOT NULL,
    cost_usd              REAL NOT NULL,             -- at model-store list price
    read_at               INTEGER NOT NULL,
    PRIMARY KEY (photo_review_id, round, model_role)
);
CREATE TABLE sightings (
    id                    TEXT PRIMARY KEY,          -- sighting_000001
    seq                   INTEGER NOT NULL UNIQUE,
    photo_review_id       TEXT NOT NULL REFERENCES photo_reviews(id),
    label                 TEXT NOT NULL,             -- a few words: "black hoodie on the chair"
    box_left              REAL NOT NULL,
    box_top               REAL NOT NULL,
    box_right             REAL NOT NULL,
    box_bottom            REAL NOT NULL,
    proposed              TEXT NOT NULL,             -- JSON, above
    versions              TEXT NOT NULL DEFAULT '{}', -- {"<model id>": its version} when the readers differ
    agreement             TEXT NOT NULL,             -- vocabulary: both_agree, readers_differ, one_reader_only
    status                TEXT NOT NULL,             -- vocabulary: proposed, accepted, rejected, same_as_other_sighting
    same_as_sighting_id   TEXT REFERENCES sightings(id), -- an earlier sighting, still waiting on the owner, of the same thing
    possession_id         TEXT REFERENCES possessions(id), -- set when accepted: a new possession or one already known
    decided_by            TEXT NOT NULL DEFAULT '',  -- principal_…, or "agreement" when both readers agreed and nothing was asked
    decided_at            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_sightings_review ON sightings(photo_review_id);
CREATE INDEX idx_sightings_same_as ON sightings(same_as_sighting_id);
CREATE TABLE owner_questions (
    id                    TEXT PRIMARY KEY,          -- possession_question_000001
    seq                   INTEGER NOT NULL UNIQUE,
    sighting_id           TEXT NOT NULL REFERENCES sightings(id),
    question              TEXT NOT NULL,
    choices               TEXT NOT NULL DEFAULT '[]',
    answer_choice         INTEGER,                   -- index into choices; NULL when unanswered or answered in words
    answer_text           TEXT NOT NULL DEFAULT '',  -- the owner's own words
    resolution            TEXT NOT NULL DEFAULT '',  -- JSON: what the answer did, once applied
    answered_by           TEXT NOT NULL DEFAULT '',
    asked_at              INTEGER NOT NULL,
    answered_at           INTEGER NOT NULL DEFAULT 0
, purpose TEXT NOT NULL DEFAULT 'decision', asked_by TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_owner_questions_open ON owner_questions(answered_at, asked_at);
CREATE INDEX idx_owner_questions_sighting ON owner_questions(sighting_id);
CREATE TABLE revisions (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    record_id             TEXT NOT NULL,             -- any id above
    record                TEXT NOT NULL,             -- the whole row as JSON, before the change
    changed_by            TEXT NOT NULL,
    changed_at            INTEGER NOT NULL
);
CREATE INDEX idx_revisions_record ON revisions(record_id, id);
CREATE TABLE furniture (
    id TEXT PRIMARY KEY,                    -- furniture_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    furniture_type TEXT NOT NULL,
    materials TEXT NOT NULL DEFAULT '[]',
    primary_color TEXT NOT NULL DEFAULT ''
);
CREATE TABLE fitness_equipment (
    id TEXT PRIMARY KEY,                    -- fitness_equipment_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    equipment_type TEXT NOT NULL,
    weight_pounds REAL
);
CREATE TABLE sports_gear (
    id TEXT PRIMARY KEY,                    -- sports_gear_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    sport TEXT NOT NULL,
    gear_type TEXT NOT NULL DEFAULT '',
    size_label TEXT NOT NULL DEFAULT ''
);
CREATE TABLE tabletop_games (
    id TEXT PRIMARY KEY,                    -- tabletop_game_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    game_type TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    publisher TEXT NOT NULL DEFAULT '',
    minimum_players INTEGER,
    maximum_players INTEGER,
    piece_count INTEGER
);
CREATE TABLE media (
    id TEXT PRIMARY KEY,                    -- media_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    media_type TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    creator TEXT NOT NULL DEFAULT '',
    platform_or_format TEXT NOT NULL DEFAULT '',
    edition TEXT NOT NULL DEFAULT ''
);
CREATE TABLE collectibles (
    id TEXT PRIMARY KEY,                    -- collectible_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    collectible_type TEXT NOT NULL,
    franchise TEXT NOT NULL DEFAULT '',
    character TEXT NOT NULL DEFAULT '',
    grade TEXT NOT NULL DEFAULT ''
);
CREATE TABLE art_and_decor (
    id TEXT PRIMARY KEY,                    -- art_and_decor_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    decor_type TEXT NOT NULL,
    subject TEXT NOT NULL DEFAULT '',
    artist TEXT NOT NULL DEFAULT '',
    framed INTEGER,
    width_inches REAL,
    height_inches REAL
);
CREATE TABLE home_textiles (
    id TEXT PRIMARY KEY,                    -- home_textile_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    textile_type TEXT NOT NULL,
    primary_color TEXT NOT NULL DEFAULT '',
    materials TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE appliances (
    id TEXT PRIMARY KEY,                    -- appliance_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    appliance_type TEXT NOT NULL,
    power_watts INTEGER
);
CREATE TABLE personal_care (
    id TEXT PRIMARY KEY,                    -- personal_care_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    care_type TEXT NOT NULL,
    powered INTEGER
);
CREATE TABLE bags_and_cases (
    id TEXT PRIMARY KEY,                    -- bag_or_case_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    bag_type TEXT NOT NULL,
    holds TEXT NOT NULL DEFAULT '',
    holds_possession_id TEXT REFERENCES possessions(id)
);
CREATE TABLE electronic_accessories (
    id TEXT PRIMARY KEY,                    -- electronic_accessory_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    accessory_type TEXT NOT NULL,
    connections TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE household_tools (
    id TEXT PRIMARY KEY,                    -- household_tool_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    tool_type TEXT NOT NULL
);
CREATE TABLE music_accessories (
    id TEXT PRIMARY KEY,                    -- music_accessory_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    accessory_type TEXT NOT NULL,
    instrument_possession_id TEXT REFERENCES possessions(id)
);
CREATE TABLE jewelry (
    id TEXT PRIMARY KEY,                    -- jewelry_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    jewelry_type TEXT NOT NULL,
    metals TEXT NOT NULL DEFAULT '[]',
    stones TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE watches (
    id TEXT PRIMARY KEY,                    -- watch_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    movement TEXT NOT NULL DEFAULT '',
    case_diameter_millimeters REAL,
    dial_color TEXT NOT NULL DEFAULT '',
    strap TEXT NOT NULL DEFAULT ''
);
CREATE TABLE eyewear (
    id TEXT PRIMARY KEY,                    -- eyewear_000001
    seq INTEGER NOT NULL UNIQUE,
    possession_id TEXT NOT NULL UNIQUE REFERENCES possessions(id),
    eyewear_type TEXT NOT NULL,
    frame_style TEXT NOT NULL DEFAULT '',
    frame_color TEXT NOT NULL DEFAULT ''
);
CREATE TABLE outfit_possessions (
    outfit_id TEXT NOT NULL REFERENCES outfits(id),
    possession_id TEXT NOT NULL REFERENCES possessions(id),
    position INTEGER NOT NULL,
    PRIMARY KEY (outfit_id, possession_id)
);
CREATE INDEX idx_outfit_possessions_possession ON outfit_possessions(possession_id);
CREATE TABLE possession_merges (
    id                    TEXT PRIMARY KEY,          -- possession_merge_000001
    seq                   INTEGER NOT NULL UNIQUE,
    duplicate_possession_id TEXT NOT NULL REFERENCES possessions(id),
    kept_possession_id    TEXT NOT NULL REFERENCES possessions(id),
    merged_by             TEXT NOT NULL,             -- principal_…, or the audit that merged it
    reason                TEXT NOT NULL DEFAULT '',  -- why: "both readers saw the same piece"
    moved                 TEXT NOT NULL,             -- JSON: possessionMergeMoves in merge.go
    merged_at             INTEGER NOT NULL,
    merge_order           INTEGER NOT NULL,          -- one count across possession_merges and outfit_merges: which came first
    undone_by             TEXT NOT NULL DEFAULT '',
    undone_at             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_possession_merges_duplicate ON possession_merges(duplicate_possession_id);
CREATE INDEX idx_possession_merges_kept ON possession_merges(kept_possession_id);
CREATE TABLE outfit_merges (
    id                    TEXT PRIMARY KEY,          -- outfit_merge_000001
    seq                   INTEGER NOT NULL UNIQUE,
    duplicate_outfit_id   TEXT NOT NULL REFERENCES outfits(id),
    kept_outfit_id        TEXT NOT NULL REFERENCES outfits(id),
    merged_by             TEXT NOT NULL,
    reason                TEXT NOT NULL DEFAULT '',
    moved                 TEXT NOT NULL,             -- JSON: outfitMergeMoves in outfit_merge.go
    merged_at             INTEGER NOT NULL,
    merge_order           INTEGER NOT NULL,          -- one count across possession_merges and outfit_merges: which came first
    undone_by             TEXT NOT NULL DEFAULT '',
    undone_at             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_outfit_merges_duplicate ON outfit_merges(duplicate_outfit_id);
CREATE INDEX idx_outfit_merges_kept ON outfit_merges(kept_outfit_id);
CREATE TABLE wearable_groups (
    id                    TEXT PRIMARY KEY,          -- wearable_group_000001
    seq                   INTEGER NOT NULL UNIQUE,
    group_key             TEXT NOT NULL UNIQUE,
    closed                INTEGER NOT NULL DEFAULT 0,
    changed_by            TEXT NOT NULL,
    changed_at            INTEGER NOT NULL
, expected_low INTEGER, expected_high INTEGER);
CREATE TABLE outfit_looks (
    id                    TEXT PRIMARY KEY,          -- outfit_look_000001
    seq                   INTEGER NOT NULL UNIQUE,
    worn_on               TEXT NOT NULL,             -- YYYY-MM-DD in the owner's time zone
    outfit_id             TEXT REFERENCES outfits(id), -- set when outfits are rebuilt from looks
    created_by            TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
, wearer_has_no_top INTEGER, not_an_outfit TEXT NOT NULL DEFAULT '');
CREATE INDEX idx_outfit_looks_outfit ON outfit_looks(outfit_id);
CREATE TABLE outfit_look_photos (
    look_id               TEXT NOT NULL REFERENCES outfit_looks(id),
    photo_id              TEXT NOT NULL UNIQUE,      -- photo-store photo_…; a selfie is in one look
    photo_review_id       TEXT NOT NULL REFERENCES photo_reviews(id),
    PRIMARY KEY (look_id, photo_id)
);
CREATE TABLE outfit_slots (
    id                    TEXT PRIMARY KEY,          -- outfit_slot_000001
    seq                   INTEGER NOT NULL UNIQUE,
    look_id               TEXT NOT NULL REFERENCES outfit_looks(id),
    slot                  TEXT NOT NULL,             -- "watch, left wrist", "jacket"
    group_key             TEXT NOT NULL,             -- the wearable group (wearableGroup)
    proposed              TEXT NOT NULL,             -- JSON Proposal: kind, kind fields, name, description
    agreement             TEXT NOT NULL,             -- vocabulary: both_agree, second_reader_changed, second_reader_added
    possession_id         TEXT REFERENCES possessions(id), -- the piece it is linked to
    decided_by            TEXT NOT NULL DEFAULT '',  -- best_guess, or who answered; '' before identification
    candidates            TEXT NOT NULL DEFAULT '[]', -- JSON [{possession_id, likelihood}], most likely first
    created_by            TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);
CREATE INDEX idx_outfit_slots_look ON outfit_slots(look_id);
CREATE INDEX idx_outfit_slots_possession ON outfit_slots(possession_id);
CREATE TABLE outfit_slot_photos (
    slot_id               TEXT NOT NULL REFERENCES outfit_slots(id),
    photo_id              TEXT NOT NULL,
    box_left              REAL NOT NULL,             -- on the photo as its thumbnail stands
    box_top               REAL NOT NULL,
    box_right             REAL NOT NULL,
    box_bottom            REAL NOT NULL,
    sighting_id           TEXT REFERENCES sightings(id), -- made when the slot is linked
    PRIMARY KEY (slot_id, photo_id)
);
CREATE TABLE outfit_rebuilds (
    id                    TEXT PRIMARY KEY,          -- outfit_rebuild_000001
    seq                   INTEGER NOT NULL UNIQUE,
    rebuilt_by            TEXT NOT NULL,
    moved                 TEXT NOT NULL,             -- JSON: outfitRebuildMoves in rebuild.go
    rebuilt_at            INTEGER NOT NULL,
    undone_by             TEXT NOT NULL DEFAULT '',
    undone_at             INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE outfit_cover_choices (
    outfit_id             TEXT PRIMARY KEY REFERENCES outfits(id),
    photo_id              TEXT NOT NULL,             -- photo-store photo_…, one of the outfit's photos when chosen
    chosen_by             TEXT NOT NULL,
    chosen_at             INTEGER NOT NULL
);
CREATE TABLE outfit_cover_wearer_boxes (
    photo_id              TEXT NOT NULL,             -- photo-store photo_…
    person_id             TEXT NOT NULL,             -- the review's wearer_person_id; '' when it names none
    box_left              REAL NOT NULL,             -- fractions of the photo as its thumbnail stands
    box_top               REAL NOT NULL,
    box_right             REAL NOT NULL,
    box_bottom            REAL NOT NULL,
    face_given            INTEGER NOT NULL,          -- 1 when the reader was told where the wearer's face is
    model                 TEXT NOT NULL,             -- the model that read it
    input_tokens          INTEGER NOT NULL,
    output_tokens         INTEGER NOT NULL,
    cost_usd              REAL NOT NULL,
    read_by               TEXT NOT NULL,             -- who asked: an actor, or "cover" for a read the outfit list started
    read_at               INTEGER NOT NULL,
    PRIMARY KEY (photo_id, person_id)
);
CREATE TABLE products (
    id                    TEXT PRIMARY KEY,          -- product_000001
    seq                   INTEGER NOT NULL UNIQUE,
    brand                 TEXT NOT NULL DEFAULT '',
    model_number          TEXT NOT NULL DEFAULT '',  -- the maker's own: SMW006A
    name                  TEXT NOT NULL,             -- the product's name on the listing
    listing_url           TEXT NOT NULL,             -- the page the description was read from
    listing_source        TEXT NOT NULL,             -- vocabulary: manufacturer, retailer, marketplace, review
    listing_publisher     TEXT NOT NULL DEFAULT '',  -- who publishes that page: "Seiko Instruments Inc."
    article_id            TEXT NOT NULL,             -- article-store article_… holding the page
    found_by              TEXT NOT NULL DEFAULT '',  -- how the listing was found, in words (the search and why this page)
    description           TEXT NOT NULL DEFAULT '',  -- the listing's own words about the product
    specs                 TEXT NOT NULL DEFAULT '[]', -- JSON [{name, value}] as the listing states them
    list_price_cents      INTEGER,                   -- NULL when the listing gives no price ("open price")
    list_price_currency   TEXT NOT NULL DEFAULT '',
    extracted_by          TEXT NOT NULL DEFAULT '',  -- the model that read description and specs from the page; '' when the caller gave them
    extraction_cost_usd   REAL NOT NULL DEFAULT 0,
    created_by            TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    deleted_at            INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_products_model ON products(lower(brand), lower(model_number)) WHERE model_number != '' AND deleted_at = 0;
CREATE INDEX idx_possessions_product ON possessions(product_id);
