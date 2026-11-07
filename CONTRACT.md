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
| possession merge | `possession_merge_…` | `/possession-merges/{id}` |
| outfit merge | `outfit_merge_…` | `/outfit-merges/{id}` |
| wearable group | `wearable_group_…` | `/wearable-groups/{group}` |

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
| `POST` | `/possessions/{id}/merge` | `{into, actor, reason}`: folds a duplicate into the possession it duplicates. The kept one takes its photos, sightings, kind (when it has none), and every field it leaves blank; outfits, installed parts, odometer readings and a harness machine move to the kept kind record. The duplicate is deleted with `merged_into_id` set. Different kinds are a **409**. Answers the kept possession with `merge_id`, the `possession_merge_…` that records it |
| `GET` | `/possession-merges` | newest first: `?possession_id=` (either side), `?limit=`, `?offset=` |
| `GET` | `/possession-merges/{id}` | `{id, duplicate_possession_id, kept_possession_id, merged_by, reason, moved, merged_at, undone_by, undone_at}`. `moved` names every row the merge changed: `filled_fields` (column → the kept one's value before), `kind_record_moved`, `installed_parts_moved`, `harness_machine_moved`, `odometer_readings_inserted`, `outfit_memberships` (`added_to_kept` when the kept one took the place), `photos_inserted`, `cover_set_photo_id`, `sightings_repointed` |
| `POST` | `/possession-merges/{id}/undo` | `{actor}`: the duplicate comes back and each moved row is put back. A merge already undone, or one a later merge (not undone) depends on — the same possession, or an outfit it changed — is a **409** naming the later one: undo in reverse order |
| `DELETE` | `/possessions/{id}/photos/{photo_id}` | the photo stays in photo-store; the earliest left becomes cover |
| `GET` | each kind's route above, without an id | possessions of that kind, with the same query as `/possessions` |
| `GET` | `/<kind route>/{id}` and `…/{id}/image` | the possession the kind record extends |
| `GET` | `/harness-machines/{machine_id}/computer` | the computer tied to a harness-store machine (`m_…`) |
| `POST` | `/vehicles/{id}/odometer-readings` | `{read_at, miles, source}` |
| `GET` | `/outfits` | `?status=`, `?possession_id=`, `?limit=`, `?offset=` |
| `POST` | `/outfits` | `{actor, name, occasion, weather, status, reasoning, preview_image_path, possession_ids:[…]}`; status is `suggested`, `kept` or `rejected`; every possession must be live and of a wearable kind (`wearable_kinds` in `/vocabulary`: garment, jewelry, watch, eyewear, bag_or_case). **201** |
| `GET` / `PATCH` | `/outfits/{id}` | an outfit answers `possession_ids`, `worn_on` (its days) and `photo_ids` (the photo-store photos outfit readings found it in); a patch changes what it gives; `possession_ids` replaces the list |
| `DELETE` | `/outfits/{id}?actor=` | soft, **204** |
| `POST` | `/outfits/{id}/wearings` | `{worn_on:"YYYY-MM-DD"}` in the owner's time zone |
| `POST` | `/outfits/{id}/combine` | `{into, actor, reason}`: folds this outfit into `into`: the kept one takes the union of pieces, every wearing and every outfit reading; this one is deleted with `merged_into_id` set. Answers the `outfit_merge` |
| `GET` | `/outfit-merges`, `/outfit-merges/{id}` | `?outfit_id=` (either side). `moved`: `pieces_added`, `wearings_added`, `reviews_repointed` |
| `POST` | `/outfit-merges/{id}/undo` | `{actor}`: the duplicate outfit comes back, and the kept one loses exactly what the merge gave it. A later merge of either outfit, or a later possession merge that moved a piece of either, is a **409** |
| `POST` | `/wearables/check-links` | `{actor}`: the link check, below. Answers an audit run |
| `POST` | `/wearables/check-duplicates` | `{actor}`: the duplicate check, below. Answers an audit run |
| `POST` | `/outfits/consolidate` | `{actor}`: outfit consolidation, below. Answers an audit run |
| `GET` | `/wearable-groups` | every wearable group with a live member or a row: `{id, group, closed, members, owned, unconfirmed, other, changed_by, changed_at}`; `owned` counts owned and lent out, `other` sold, given away, lost and discarded; `id` is null for a group never set |
| `PUT` | `/wearable-groups/{group}` | `{closed: true\|false, actor}`: the owner declares a group complete, or opens it again. A group is `watch`, `eyewear`, `garment:<category>`, `jewelry:<jewelry type>` or `bag_or_case:<bag type>`; any other is a **400**. Answers the group |
| `POST` | `/outfit-pieces/rematch` | `{actor}`: the rematch, below. Answers an audit run |
| `GET` / `POST` | `/purchases` | `{seller, marketplace, order_reference, ordered_at, total_cents, currency, source, lines:[{title, quantity, unit_price_cents, product_reference, disposition}]}`. An order already recorded (same marketplace and reference) is a **409** |
| `GET` | `/purchases/{id}` | with lines; each line lists the possessions that point at it (`purchase_line_id`) |
| `PATCH` | `/purchase-lines/{id}` | `{disposition, actor}` |
| `GET` / `POST` | `/photo-reviews` | `POST {photo_id, queued_by, reading, wearer_person_id}` queues a photo-store photo for a reading, `inventory` (everything in a room, two readers) or `outfit` (what `wearer_person_id`, a people-store person confirmed on the photo, has on; one reader): **201**, or **200** with the review already made for that photo |
| `GET` | `/photo-reviews/{id}` | with every reading (each model's answer unchanged, tokens, cost) and sighting; `cost_usd` sums the readings |
| `POST` | `/photo-reviews/{id}/retry` | `{actor}`: a `failed` review only; its readings go to revisions and it is queued again |
| `GET` | `/sightings/{id}` and `/sightings/{id}/image` | |
| `GET` | `/owner-questions` | open ones, oldest first; `?include_answered=true`, `?photo_review_id=`, `?purpose=decision\|detail`. Each says `asked_by`: `""` when the photo's reading asked it, otherwise `link_check`, `duplicate_check`, `outfit_consolidation`, `picture_match` or `closed_group` |
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

A reading's picture match (below) is saved as readings too: round 1 is the
outfit reading, and each piece compared is one round more, both readers.

## Picture match in outfit readings

After the reader lists the pieces, each wearable piece is cut from the photo
and shown to **both** readers beside the covers of up to 12 recorded
wearables of its kind and sub-type (a garment's category, a jewelry type, a
bag type; watches and eyewear are one group each), nearest by their words,
the reader's own match always among them. Each picks one or `none`. Both
pick the same piece: the sighting is that piece. Both pick none: a new
piece (`unconfirmed`), or the earlier sighting the reader named. They
differ: a decision question (`asked_by: picture_match`) with each pick, "a
piece not recorded yet" and "not mine". A doubt the reader wrote follows a
recorded piece as a detail question. A piece with no recorded piece of its
group to compare skips the match. Then the reading's pieces find the outfit
with exactly those pieces; failing that, the outfit worn on the same day
that shares the most pieces with them, at least one, which takes the new
pieces; failing that, a new outfit.

## Wearable audit

Three passes over the wearables and outfits already recorded. Each shows
pictures to both reader roles; where both agree it acts at once, and every
merge it makes is an undoable `possession_merge` or `outfit_merge`; where
they differ it asks a decision question on a sighting, so the owner sees a
picture. A pair or link asked about once, answered or not, is not asked
again. Each change is its own transaction with revisions. A pass runs until
done, so call it with a long timeout, and answers:

```json
{"looked":…,"counts":{"merged":…,"asked":…},"actions":[{"action":"merged","records":["possession_…","possession_…"],"merge_id":"possession_merge_…","question_id":"","reason":"…"}],
 "left":[{"about":…,"why":…}],"failures":[{"about":…,"error":…}],"cost_usd":…}
```

A pass that stops on an error after acting answers the error with
`done_before_the_error` holding the run so far.

**Link check** (`POST /wearables/check-links`). For each live wearable with
two or more photos, both readers see its cover crop and each other linked
crop (photo-store's thumbnail cut by the stored box), at most 19 a call, and
say per crop whether it is the same physical piece, naming other recorded
pieces of its group it could be. Both yes: `confirmed`. Both no:
`unlinked` — the photo link, the sighting (back to `proposed`) and the
piece's place in that reading's outfit (unless another photo of the outfit
shows it) are undone, and a decision question asks: a piece not recorded
yet, one of the pieces the readers named, or not mine. One no: `asked` — the
link stays and a question offers `keep_link`, or a different piece (new,
named, or not mine) whose resolution carries `unlink_possession_id`. A link
a person made (no sighting) is left.

**Duplicate check** (`POST /wearables/check-duplicates`). Live wearables
with a cover are grouped by kind and sub-type. Both readers see each group's
crops, labeled, and list clusters of one physical piece. A group of more
than 20 is cut into chunks of 10 and every two chunks are shown together,
so every pair is seen side by side at most 20 pictures a call (31 pieces:
6 calls a reader). A pair both readers cluster in every call that showed
both is merged into the one with more photos, then the older; pairs join a
set only when every pair across the sets agreed. A pair only one clusters
is a question with both pieces, choices `merge_possessions` and
`keep_apart`; the question's text names both `/possessions/{id}/image`.

**Outfit consolidation** (`POST /outfits/consolidate`). First, outfits
holding exactly the same pieces are combined without asking (into the one
worn on more days, then the older). Then each pair of live outfits worn on
a common day, or whose pieces overlap by half or more (shared over all), is
shown to both readers: up to 3 selfies of each and the covers of the pieces
only one holds. Each says whether it is the same outfit and maps each of
outfit A's own pieces to one of B's own, `none` (B's photos show the place
and it is not there) or `not_visible`. Only when both say the same outfit
and map every piece alike, none `none`, one to one and kind to kind, are
the mapped pieces merged and the outfits combined, in one transaction. Both
say different: nothing. Anything else is a question listing what differs,
choices `combine_outfits` (with the piece merges both agreed on) and
`keep_outfits_apart`. Pairs are compared again after any combine.

An answer in words to an audit question goes to the first reader, which
picks one of its choices; words that fit none are a **400**.

## Closed groups

The owner can declare a wearable group complete (`PUT /wearable-groups/{group}
{closed:true}`): every watch they own, every ring, is recorded. In a closed
group nothing is recorded as new, whatever asks:

- **Picture match** (new readings and the rematch): both readers see every
  owned member (owned or lent out) with up to 6 of its photos, cover first,
  not the 12 nearest covers. A call holds at most 30 pictures; a group with
  more is shown in batches, the piece in each, and a reader's pick is the one
  member it named across the batches (`not_here` in the others); naming two
  is `cannot_tell`. Each must pick a member or say `cannot_tell`. Both pick
  the same member: linked. Otherwise a decision question whose choices are
  every member (`/possessions/{id}/image` draws each) and "not mine".
- **Accepting a new piece** whose kind and sub-type make a closed group is a
  **409**, so an older question's "a piece not recorded yet" is refused too.
- **Link check**: a question about a doubted link offers the other owned
  members and "not mine", never a new piece.
- **Duplicate check**: only the members that are not `unconfirmed` are
  clustered; each `unconfirmed` member is matched as in the rematch's second
  step.
- **Merges** keep an owned possession over an unconfirmed one, before photo
  count and age.

## Rematch

`POST /outfit-pieces/rematch {actor}` runs the picture match again where
nothing settled:

1. Each outfit-reading sighting still `proposed` behind an open decision
   question is cut from the photo the reader saw and matched (closed-group
   rules where its group is closed; a possession the link check unlinked it
   from is left out). Both readers agree: the sighting is settled as they say
   (a member, or in an open group a new `unconfirmed` piece when both say
   none), each of its open decision questions is answered with
   `answered_by: picture_match`, the resolution, and `answer_choice` when a
   choice says the same, the piece joins its reading's outfit, and the review
   is `done` once no decision is open. They differ: the question stays
   (`disagreed`). The calls are saved as readings of the review.
2. Each `unconfirmed` possession in a closed group: its photos (up to 6) are
   matched against the owned members. Both readers pick one member: an
   undoable merge into it (`merged`). Otherwise a decision question
   (`asked_by: closed_group`) offers `merge_possessions` into each member and
   `keep_apart` ("none of these"). One already asked about is left.

Counts: `matched`, `disagreed`, `settled_meanwhile`, `merged`, `asked`.
