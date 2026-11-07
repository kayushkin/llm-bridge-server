# possession-store — API contract

`127.0.0.1:8341`, loopback only, no auth: dash is the front door. An inventory
of the things a person owns. SQLite at `<data dir>/possession-store.db`
(default `~/.config/possession-store`, directory 0700, file 0600); the
originals `possession-add-photos` brings in sit in `<data dir>/originals/`,
named by their sha256. Timestamps are epoch seconds. Money is integer cents
with an ISO 4217 code.

## Settings

`GET /settings` lists them. The service reads:

| Variable | Default | What it is |
|---|---|---|
| `POSSESSION_STORE_ADDR` | `127.0.0.1:8341` | listen address |
| `POSSESSION_STORE_DATA_DIR` | `~/.config/possession-store` | database and originals |
| `PHOTO_STORE_URL` | `http://127.0.0.1:8329` | photos are checked and read there |
| `LOCATION_STORE_URL` | `http://127.0.0.1:8333` | a `place_id` is checked there |
| `POSSESSION_STORE_HARNESS_STORE_DATABASE` | `~/.config/harness-store/harness.db` | read-only: a computer's `harness_machine_id` is checked in its `machines` |
| `LLMBRIDGE_URL` | `http://localhost:8160` | the readers are asked through `POST /oneshot` |
| `LLMBRIDGE_SERVICE_TOKEN` | (secret) | required while the review worker is on; a host-local drop-in |
| `MODEL_STORE_URL` | `http://localhost:8155` | list prices for a reading's cost |
| `POSSESSION_STORE_FIRST_READER_ROLE` | `possession-photo-reader` | model-store role of the first reader; it also reads answers given in words |
| `POSSESSION_STORE_SECOND_READER_ROLE` | `possession-photo-second-reader` | model-store role of the second reader |
| `POSSESSION_STORE_REVIEW_WORKER_ENABLED` | `true` | off, queued photos wait |
| `POSSESSION_STORE_OWNER_TIME_ZONE` | `UTC` | the owner's IANA zone: an outfit read from a photo was worn on the photo's day there; set it in a host-local drop-in |
| `POSSESSION_STORE_REVIEW_IMAGE_LONGEST_SIDE` | `2048` | size of the original the readers see; without an original they see photo-store's 1024-pixel thumbnail |

## Records and ids

Every id is `<prefix>_000001`. kanban-store's registry claims each prefix, so
a chat draws an id as a chip.

| Record | Id | Route |
|---|---|---|
| possession | `possession_…` | `/possessions/{id}` |
| garment | `garment_…` | `/garments/{id}` |
| computer | `computer_…` | `/computers/{id}` |
| computer part | `computer_part_…` | `/computer-parts/{id}` |
| audio device | `audio_device_…` | `/audio-devices/{id}` |
| musical instrument | `musical_instrument_…` | `/musical-instruments/{id}` |
| vehicle | `vehicle_…` | `/vehicles/{id}` |
| furniture | `furniture_…` | `/furniture/{id}` |
| fitness equipment | `fitness_equipment_…` | `/fitness-equipment/{id}` |
| sports gear | `sports_gear_…` | `/sports-gear/{id}` |
| tabletop game | `tabletop_game_…` | `/tabletop-games/{id}` |
| media | `media_…` | `/media/{id}` |
| collectible | `collectible_…` | `/collectibles/{id}` |
| art and decor | `art_and_decor_…` | `/art-and-decor/{id}` |
| home textile | `home_textile_…` | `/home-textiles/{id}` |
| appliance | `appliance_…` | `/appliances/{id}` |
| personal care | `personal_care_…` | `/personal-care/{id}` |
| bag or case | `bag_or_case_…` | `/bags-and-cases/{id}` |
| electronic accessory | `electronic_accessory_…` | `/electronic-accessories/{id}` |
| household tool | `household_tool_…` | `/household-tools/{id}` |
| music accessory | `music_accessory_…` | `/music-accessories/{id}` |
| jewelry | `jewelry_…` | `/jewelry/{id}` |
| watch | `watch_…` | `/watches/{id}` |
| eyewear | `eyewear_…` | `/eyewear/{id}` |
| outfit | `outfit_…` | `/outfits/{id}` |
| purchase | `possession_purchase_…` | `/purchases/{id}` |
| purchase line | `purchase_line_…` | inside its purchase |
| photo review | `photo_review_…` | `/photo-reviews/{id}` |
| sighting | `sighting_…` | `/sightings/{id}` |
| question | `possession_question_…` | `/owner-questions/{id}` |

A **possession** holds what every owned thing has. A **kind** (garment,
computer…) is a table of its own with only that kind's facts, one row per
possession. A possession with no kind is complete as it is. `GET /kinds` lists
every kind's columns, types and vocabularies; `GET /vocabulary` every closed
word list. Nothing a person can cite is hard-deleted, and every change first
copies the whole row to `revisions`.

### Possession, as every read answers it

```json
{"id":"possession_000001","name":"black hoodie","room":"bedroom","brand":"Carhartt","model":"","serial_number":"",
 "description":"","quantity":1,"condition":"","status":"owned","place_id":null,"spot":"bedroom closet",
 "acquired_at":0,"acquired_how":"","purchase_line_id":null,"price_cents":null,"price_currency":"",
 "disposed_at":0,"tags":[],"created_by":"principal_000001","created_at":…,"updated_at":…,"deleted_at":0,"merged_into_id":null,
 "kind":"garment",
 "garment":{"id":"garment_000001","category":"top","garment_type":"hoodie","layer":"mid","primary_color":"black",
            "secondary_colors":[],"pattern":"solid","materials":["cotton"],"size_label":"L","size_system":"letter",
            "fit":"relaxed","formality":2,"warmth":4,"seasons":["fall","winter"]},
 "outfit_ids":["outfit_000001"],
 "photos":[{"photo_id":"photo_000123","box_left":0.1,"box_top":0.2,"box_right":0.5,"box_bottom":0.7,
            "is_cover":true,"sighting_id":"sighting_000004","linked_by":"agreement","linked_at":…}],
 "cover_photo_id":"photo_000123"}
```

`kind` is `""` and no kind key is present for a possession with no kind. A
computer also carries `installed_part_ids`; a vehicle `odometer_readings`.

## Routes

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{possessions, outfits, purchases, photo_reviews_queued, photo_reviews_failed, open_decision_questions, open_detail_questions}}` |
| `GET` | `/vocabulary`, `/kinds`, `/settings` | |
| `GET` | `/possessions` | newest first: `?q=` (every word in name, brand, model, description, spot, tags), `?status=`, `?kind=` (a kind, or `none`), `?place_id=`, `?include_deleted=true`, `?limit=` (default 100, max 1000), `?offset=` |
| `POST` | `/possessions` | `{"actor":…, <possession fields>, "<kind>":{<kind fields>}, "photos":[{photo_id, box_left…, is_cover}]}`. `name` is required; `status` defaults to `owned`; at most one kind. An unknown field, a word outside a vocabulary, a missing required kind field, an id the owning store does not have, or a price with no currency is a **400**. **201** |
| `GET` | `/possessions/{id}` | `?include_deleted=true` |
| `PATCH` | `/possessions/{id}` | `{"actor":…, <fields>, "<kind>":{<fields>}}`: only what is given changes. A possession with no kind takes one; one with another kind is a **409**. Photos change through the photo routes |
| `DELETE` | `/possessions/{id}?actor=` | soft, **204**; `POST /possessions/{id}/restore {"actor"}` brings it back |
| `GET` | `/possessions/{id}/revisions` | newest first, each the whole row as it was |
| `GET` | `/possessions/{id}/image` | JPEG: the possession cut from its cover photo's thumbnail by its box, with a margin; the whole thumbnail when the photo has no box. **404** with no photo |
| `POST` | `/possessions/{id}/photos` | `{photo_id, box_left, box_top, box_right, box_bottom, is_cover, linked_by}`: box sides are fractions 0–1, all four or none. The first photo is the cover until another is chosen. Linking again changes the box or cover |
| `POST` | `/possessions/classify-kindless` | `{actor, batch_size}` (1–20): the first reader looks at every live possession with no kind, batch_size at a time with their pictures. A sure answer gives it the kind and its fields (and a room when it has none); an unsure one raises a detail question on its sighting whose first choice gives it that kind. Answers `{looked, classified, asked, left:[{possession_id, why}], cost_usd}`. Runs until done, so call it with a long timeout |
| `PUT` | `/possessions/{id}/kind` | `{kind, fields, actor}`: moves a possession to another kind; its old kind record goes to revisions. An empty kind leaves it with none. A computer with installed parts, or a wearable in an outfit moving to a kind that is not wearable, is a **409** |
| `POST` | `/possessions/{id}/merge` | `{into, actor}`: folds a duplicate into the possession it duplicates. The kept one takes its photos, sightings, kind (when it has none), and every field it leaves blank; outfits, installed parts, odometer readings and a harness machine move to the kept kind record. The duplicate is deleted with `merged_into_id` set. Different kinds are a **409** |
| `DELETE` | `/possessions/{id}/photos/{photo_id}` | the photo stays in photo-store; the earliest left becomes cover |
| `GET` | each kind's route above, without an id | possessions of that kind, with the same query as `/possessions` |
| `GET` | `/<kind route>/{id}` and `…/{id}/image` | the possession the kind record extends |
| `GET` | `/harness-machines/{machine_id}/computer` | the computer tied to a harness-store machine (`m_…`) |
| `POST` | `/vehicles/{id}/odometer-readings` | `{read_at, miles, source}` |
| `GET` | `/outfits` | `?status=`, `?possession_id=`, `?limit=`, `?offset=` |
| `POST` | `/outfits` | `{actor, name, occasion, weather, status, reasoning, preview_image_path, possession_ids:[…]}`; status is `suggested`, `kept` or `rejected`; every possession must be live and of a wearable kind (`wearable_kinds` in `/vocabulary`: garment, jewelry, watch, eyewear, bag_or_case). **201** |
| `GET` / `PATCH` | `/outfits/{id}` | a patch changes what it gives; `possession_ids` replaces the list |
| `DELETE` | `/outfits/{id}?actor=` | soft, **204** |
| `POST` | `/outfits/{id}/wearings` | `{worn_on:"YYYY-MM-DD"}` in the owner's time zone |
| `GET` / `POST` | `/purchases` | `{seller, marketplace, order_reference, ordered_at, total_cents, currency, source, lines:[{title, quantity, unit_price_cents, product_reference, disposition}]}`. An order already recorded (same marketplace and reference) is a **409** |
| `GET` | `/purchases/{id}` | with lines; each line lists the possessions that point at it (`purchase_line_id`) |
| `PATCH` | `/purchase-lines/{id}` | `{disposition, actor}` |
| `GET` / `POST` | `/photo-reviews` | `POST {photo_id, queued_by, reading, wearer_person_id}` queues a photo-store photo for a reading, `inventory` (everything in a room, two readers) or `outfit` (what `wearer_person_id`, a people-store person confirmed on the photo, has on; one reader): **201**, or **200** with the review already made for that photo |
| `GET` | `/photo-reviews/{id}` | with every reading (each model's answer unchanged, tokens, cost) and sighting; `cost_usd` sums the readings |
| `POST` | `/photo-reviews/{id}/retry` | `{actor}`: a `failed` review only; its readings go to revisions and it is queued again |
| `GET` | `/sightings/{id}` and `/sightings/{id}/image` | |
| `GET` | `/owner-questions` | open ones, oldest first; `?include_answered=true`, `?photo_review_id=`, `?purpose=decision\|detail` |
| `POST` | `/owner-questions/triage-outfit-doubts` | `{actor}`: for outfit readings made before readers labeled their doubts. The first reader sorts each open decision question by its text: about which piece it is (stays a decision) or only a fact about a clear piece (the piece is recorded as the reader proposed, joins its outfit, and the question becomes a detail). Answers `{looked, identity, detail, failures, cost_usd}` |
| `GET` | `/owner-questions/{id}` and `…/image` | the image is the sighting cut from its photo |
| `POST` | `/owner-questions/{id}/answer` | `{choice:<index>}` or `{text:"…"}`, and `answered_by`. Words go to the first reader, which turns them into a resolution before anything is written: for a detail question, the possession as it should now read (`amend`). A second answer is a **409** |
| `POST` | `/sightings/record-agreed` | `{actor}`: records every sighting both readers agreed on that still waits on a decision question, and makes that question a detail question. For reviews made before agreed sightings were recorded at once. Answers `{recorded, failures:[{sighting_id, error}]}`; running it again records nothing new |

## Photo review

`possession-add-photos -queued-by principal_… [-folder possessions]
[-time-zone America/Los_Angeles] photo.jpg…` copies each original into
`originals/`, adds it to photo-store (upright, with its EXIF date and GPS;
`source_machine` is harness-store's one local machine), and queues it. It
prints `path, photo id, review id` per photo.

The worker takes one queued photo at a time:

1. **Round one.** Both readers (the two roles) read the photo on their own and
   list each possession with a box, name, brand and model only when readable,
   a kind and kind fields.
2. **Round two.** Each reader sees both lists (labelled `A1…`, `B1…`) and what
   the inventory holds (live possessions, newest 600, and sightings from other
   photos still waiting on the owner), and writes its final list: entries
   merged, `existing_id` when it is something already recorded, and a `doubt`
   when it would ask the owner.
3. **Agreement.** The store pairs the two final lists by shared round-one
   labels, then by overlapping boxes. A pair with the same kind, brand, model,
   kind words, quantity and match is recorded at once: a new possession
   (`created_by` the review), the photo added to a matched one, or a wait on
   an earlier sighting. A doubt either reader raised about such a pair
   becomes a **detail** question: optional, answered with "leave it as
   recorded" or in words, which amend the possession. Everything else waits on
   a **decision** question whose choices say what each answer does (`accept`,
   `same_as`, `same_as_sighting`, `reject`).

A kind field a reader gives that does not fit the column is dropped and named
in the proposal's `dropped_fields`, and the new possession's description keeps
it in words. When a sighting is settled, every later sighting waiting on it is
settled the same way. A review is `waiting_for_owner` while any of its
decision questions is open, then `done`.

## Outfit reading

A review with `reading: outfit` asks the first reader alone what the wearer
has on, given where their face is (photo-store's confirmed face) and every
wearable already recorded. A piece it is sure of is recorded at once: matched
to its possession, or a new possession with status `unconfirmed` and a detail
question, "Do you still have it?", whose choices set its status. A piece it
doubts waits on a decision question when the doubt is which piece it is
(`doubt_kind: identity`); a doubt only about a fact of a clear piece
(`doubt_kind: detail`) records the piece and asks a detail question. The recorded pieces make an outfit
(`created_by` the review), or find the live outfit holding exactly the same
pieces, and get a wearing on the photo's day in the owner's time zone
(`worn_on` on the review). A piece accepted later, by an answer or by a
sighting it waited on, joins that outfit.
