# possession-store — API contract

An inventory of the things a person owns, served by llm-bridge-server at
`/possession-store/` with these paths behind the prefix. Timestamps are epoch
seconds. Money is integer cents with an ISO 4217 code.

The records live in Postgres, in the module `possessions` (role
`possessions_module`). The originals `possession-add-photos` brings in are
files in file-store, named in table `originals` by the sha256 of their bytes.

## Owners

Every record has an owner, `owner_principal_id`, principal-store's id of the
person who keeps it; a possession and an outfit answer it. A request reaches
the records of one scope, from the caller the server verified:

- A `GET` reaches the caller's own records; an administrator's, or this
  server's own service's, reaches every owner's, or one owner's named in
  `?owner_principal_id=`.
- Anything else acts for one owner: the caller, or the owner an
  administrator names in `?owner_principal_id=`. The service token naming no
  one is **403** there. A record of another owner answers **404**, as a
  missing one does, and every store asked on the way (photo-store,
  location-store, mailstack) is asked as that owner.

Postgres holds the scope too: each connection names its owner, and
row-level security shows and writes no other owner's rows. Ids are numbered
across owners, so `possession_000002` may be another owner's.

## Settings

The store reads llm-bridge-server's settings (`GET /settings` there):

| Setting | Default | What it is |
|---|---|---|
| `possession_store.review_worker.enabled` | `false` | review queued photos, and read covers' wearer boxes in the background; off, queued photos wait |
| `possession_store.review_worker.owner_principal_ids` | empty | the owners whose photos the worker reviews, each as that owner |
| `possession_store.first_reader_role` | `possession-photo-reader` | model-store role of the first reader; it also reads answers given in words |
| `possession_store.second_reader_role` | `possession-photo-second-reader` | model-store role of the second reader |
| `possession_store.owner_time_zone` | `UTC` | an outfit read from a photo was worn on the photo's day in this zone |
| `possession_store.review_image_longest_side` | `2048` | size of the original the readers see; without one they see photo-store's 1024-pixel thumbnail |
| `possession_store.cover_cache_directory` | under the server's folder | outfit covers already cut; any file may be deleted |
| `possession_store.article_store_url` | empty | article-store, outside the server: product listings are saved and read there |

Model calls go through the server's one-shot router, in process, priced at
model-store's list price. photo-store, location-store, harness-store,
mailstack and budget-store (eBay orders, `budget_transaction_id` checks,
order-line links) are called in process, as the owner the call is for.

## Records and ids

Every id is `<prefix>_000001`. reference-store's type list claims each prefix,
so a chat draws an id as a chip.

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
| product | `product_…` | `/products/{id}` |
| photo review | `photo_review_…` | `/photo-reviews/{id}` |
| sighting | `sighting_…` | `/sightings/{id}` |
| question | `possession_question_…` | `/owner-questions/{id}` |
| possession merge | `possession_merge_…` | `/possession-merges/{id}` |
| outfit merge | `outfit_merge_…` | `/outfit-merges/{id}` |
| wearable group | `wearable_group_…` | `/wearable-groups/{group}` |
| look | `outfit_look_…` | `/outfit-looks/{id}` |
| slot | `outfit_slot_…` | `/outfit-slots/{id}` |
| outfit rebuild | `outfit_rebuild_…` | `/outfit-rebuilds/{id}` |

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
 "acquired_at":0,"acquired_how":"","purchase_line_id":null,"product_id":null,"budget_transaction_id":null,"price_cents":null,"price_currency":"",
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

`kind` is `""` and no kind key is present for a possession with no kind.
`product` is null, or the product `product_id` names, read with it: `{id,
brand, model_number, name, listing_url, listing_source, listing_publisher,
article_id, description, list_price_cents, list_price_currency}`. A
computer also carries `installed_part_ids`; a vehicle `odometer_readings`.

## Routes

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{possessions, outfits, purchases, photo_reviews_queued, photo_reviews_failed, open_decision_questions, open_detail_questions}}` |
| `GET` | `/vocabulary`, `/kinds` | |
| `PUT` | `/originals/{sha256}?extension=.jpg&added_by=…` | the body is an original's bytes, whose sha256 must be the path's; kept in file-store for the owner the request acts for. **201**, or **200** with `created:false` when that owner keeps it already. Answers `{original, created, source_machine, source_path}`: what photo-store should record for the photo |
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
| `GET` | `/possession-merges/{id}` | `{id, duplicate_possession_id, kept_possession_id, merged_by, reason, moved, merged_at, undone_by, undone_at}`. `moved` names every row the merge changed: `filled_fields` (column → the kept one's value before), `kind_record_moved`, `installed_parts_moved`, `harness_machine_moved`, `odometer_readings_inserted`, `outfit_memberships` (`added_to_kept` when the kept one took the place), `photos_inserted`, `cover_set_photo_id`, `sightings_repointed`, `slots_repointed` (outfit slots linked to the duplicate) |
| `POST` | `/possession-merges/{id}/undo` | `{actor}`: the duplicate comes back and each moved row is put back. A merge already undone, or one a later merge (not undone) depends on — the same possession, or an outfit it changed — is a **409** naming the later one: undo in reverse order |
| `DELETE` | `/possessions/{id}/photos/{photo_id}` | the photo stays in photo-store; the earliest left becomes cover |
| `GET` | each kind's route above, without an id | possessions of that kind, with the same query as `/possessions` |
| `GET` | `/<kind route>/{id}` and `…/{id}/image` | the possession the kind record extends |
| `GET` | `/harness-machines/{machine_id}/computer` | the computer tied to a harness-store machine (`m_…`) |
| `POST` | `/vehicles/{id}/odometer-readings` | `{read_at, miles, source}` |
| `GET` | `/outfits` | `?status=`, `?possession_id=`, `?favorite=true`, `?tag=` (one tag, tidied as below), `?limit=`, `?offset=` |
| `GET` | `/outfit-tags` | every tag on a live outfit with its count, most used first: `[{tag, outfits}]` |
| `POST` | `/outfits` | `{actor, name, occasion, weather, status, reasoning, preview_image_path, favorite, tags, possession_ids:[…]}`; status is `suggested`, `kept` or `rejected`; every possession must be live and of a wearable kind (`wearable_kinds` in `/vocabulary`: garment, jewelry, watch, eyewear, bag_or_case). **201** |
| `GET` / `PATCH` | `/outfits/{id}` | an outfit answers `possession_ids`, `favorite`, `tags`, `worn_on` (its days), `photo_ids` (the photo-store photos outfit readings found it in) and its cover: `cover_photo_id`, `cover_box`, `cover_aspect` and `cover_reason` (below); a patch changes what it gives; `possession_ids` and `tags` each replace the list. `favorite` is the owner's mark; `tags` are the owner's words, each trimmed, its spaces made one, lower case and kept once, in the order given; an empty tag or one over 40 characters is a **400** |
| `PUT` | `/outfits/{id}/cover` | `{photo_id, actor}`: the owner's pick of the cover, one of the outfit's `photo_ids` (another is a **400**); `""` takes the pick away so the rule chooses again. The new cover photo's wearer box is read at once when it has none; when that read fails the pick stays and the answer is the error. Answers the outfit |
| `POST` | `/outfit-covers/read-wearer-boxes` | `{actor}`: reads the wearer box of every live outfit's cover photo that has none, four at a time. Answers `{cover_photos, already_read, read, failed:[{photo_id, why}], cost_usd}` |
| `GET` | `/outfits/{id}/image` | JPEG: the cover photo cut to `cover_box` (`cover_aspect` wide over tall), at most 900 pixels tall, from the original when its owner keeps one, else the thumbnail. **404** with no photo |
| `DELETE` | `/outfits/{id}?actor=` | soft, **204** |
| `POST` | `/outfits/{id}/wearings` | `{worn_on:"YYYY-MM-DD"}` in the owner's time zone |
| `POST` | `/outfits/{id}/combine` | `{into, actor, reason}`: folds this outfit into `into`: the kept one takes the union of pieces, every wearing, every outfit reading, the tags it lacked, and favorite when this one was; this one is deleted with `merged_into_id` set. Answers the `outfit_merge` |
| `GET` | `/outfit-merges`, `/outfit-merges/{id}` | `?outfit_id=` (either side). `moved`: `pieces_added`, `wearings_added`, `reviews_repointed`, `favorite_set`, `tags_added` |
| `POST` | `/outfit-merges/{id}/undo` | `{actor}`: the duplicate outfit comes back, and the kept one loses exactly what the merge gave it. A later merge of either outfit, or a later possession merge that moved a piece of either, is a **409** |
| `POST` | `/wearables/check-links` | `{actor}`: the link check, below. Answers an audit run |
| `POST` | `/wearables/check-duplicates` | `{actor}`: the duplicate check, below. Answers an audit run |
| `POST` | `/outfits/consolidate` | `{actor}`: outfit consolidation, below. Answers an audit run |
| `GET` | `/wearable-groups` | every wearable group with a live member or a row: `{id, group, closed, expected_low, expected_high, members, owned, unconfirmed, other, changed_by, changed_at}`; `owned` counts owned and lent out, `other` sold, given away, lost and discarded; `id` is null for a group never set |
| `PUT` | `/wearable-groups/{group}` | `{closed, expected_low, expected_high, actor}`, each optional but one required: closes or opens a group, and sets how many individual pieces the owner says it holds. A group is `watch`, `eyewear`, `garment:<category>`, `garment:scarf\|belt\|gloves\|tie`, `jewelry:<jewelry type>` (pendants and chains are `jewelry:necklace`) or `bag_or_case:<bag type>`; any other, or a range upside down, is a **400**. Answers the group |
| `POST` | `/outfit-looks/read` | `{actor}`: reads looks and slots from every day's selfies not yet in a look, below. Answers an audit run |
| `GET` | `/outfit-looks` | by day: `?worn_on=`, `?outfit_id=`, `?limit=`, `?offset=`. A look: `{id, worn_on, outfit_id, wearer_has_no_top, not_an_outfit, photo_ids, slots, created_by, created_at, updated_at}`; `wearer_has_no_top` is the second reader's word (null for a look read before it was asked), `not_an_outfit` why the last rebuild left it out of every outfit (`""` otherwise) |
| `GET` | `/outfit-looks/{id}`, `/outfit-slots/{id}` | a slot: `{id, look_id, slot, group, proposed, agreement, possession_id, decided_by, candidates:[{possession_id, likelihood}], photos:[{photo_id, box, sighting_id}]}` |
| `GET` | `/outfit-slots/{id}/image` | JPEG: the slot cut from the full-size photo where its box is largest, as identification shows it |
| `POST` | `/outfit-slots/identify` | `{actor}`: identification, below. An audit run with `groups:[{group, expected_low, expected_high, slots, pieces, identified, within_range, pull_in}]`; `pull_in` is `{before, shown, merged, after}` for a group that held more than its expected high, null otherwise |
| `POST` | `/outfits/rebuild-from-looks` | `{actor}`: rebuilds outfits from looks, below. An audit run; its last action names the `outfit_rebuild` |
| `GET` | `/outfit-rebuilds/{id}` | `{id, rebuilt_by, moved:{outfits_made, outfits_replaced, pieces_added, looks_marked, pieces_removed, wearings_removed, wearings_added, looks, reviews}, rebuilt_at, undone_by, undone_at}`; `pieces_added` and `wearings_added` are what it gave outfits an earlier rebuild made, `pieces_removed` and `wearings_removed` what it took from them when a look left; `looks_marked` is `[{look_id, before, after}]` of `not_an_outfit`; a look or reading move to `new_outfit_id: ""` took it out of every outfit |
| `POST` | `/outfit-rebuilds/{id}/undo` | `{actor}`: puts back every outfit, look and reading the rebuild changed. A later rebuild not undone, a look that has moved since (a slot answer), or a merge of an outfit it made is a **409** |
| `POST` | `/outfit-pieces/rematch` | `{actor}`: the rematch, below. Answers an audit run |
| `GET` / `POST` | `/purchases` | `{seller, marketplace, order_reference, ordered_at, total_cents, currency, source, lines:[{title, quantity, unit_price_cents, product_reference, disposition}]}`. An order already recorded (same marketplace and reference) is a **409**. eBay orders are not purchases here: they are budget-store marketplace orders (below) |
| `DELETE` | `/purchases/{id}?actor=&reason=` | soft, with a revision; a possession whose `purchase_line_id` is one of its lines is a **409**. Every open question about one of its lines is withdrawn: answered by `actor`, `answer_text` "withdrawn: <reason>", resolution `withdrawn`. Answers `{deleted, withdrawn_question_ids}` |
| `POST` | `/marketplace-orders/import-ebay-mail` | `{actor, account, query}`: the eBay import, below. Answers an audit run |
| `POST` | `/marketplace-order-lines/match` | `{actor, marketplace_order_id}` (the order optional): the purchase match, below. Answers an audit run |
| `GET` / `POST` | `/products` | `GET ?q=` (every word in brand, model number, name, description), `?limit=`, `?offset=`. `POST`: below. **201** |
| `GET` / `PATCH` | `/products/{id}` | a product with `possession_ids`; a patch takes `actor` and any of brand, model_number, name, listing_url, listing_source, listing_publisher, article_id (checked in article-store), found_by, description, specs, list_price_cents, list_price_currency. A possession points at its product with `PATCH /possessions/{id} {"product_id":…}` |
| `GET` | `/purchases/{id}` | with lines; each line lists the possessions that point at it (`purchase_line_id`) |
| `PATCH` | `/purchase-lines/{id}` | `{disposition, actor}` |
| `GET` / `POST` | `/photo-reviews` | `POST {photo_id, queued_by, reading, wearer_person_id}` queues a photo-store photo for a reading, `inventory` (everything in a room, two readers) `outfit` (what `wearer_person_id`, a people-store person confirmed on the photo, has on; one reader) or `look` (a photo of `wearer_person_id` waiting for `POST /outfit-looks/read`; the review worker does not read it): **201**, or **200** with the review already made for that photo |
| `GET` | `/photo-reviews/{id}` | with every reading (each model's answer unchanged, tokens, cost) and sighting; `cost_usd` sums the readings |
| `POST` | `/photo-reviews/{id}/retry` | `{actor}`: a `failed` review only; its readings go to revisions and it is queued again |
| `GET` | `/sightings/{id}` and `/sightings/{id}/image` | |
| `GET` | `/owner-questions` | open ones, oldest first; `?include_answered=true`, `?photo_review_id=`, `?purpose=decision\|detail`. Each says `asked_by`: `""` when the photo's reading asked it, otherwise `link_check`, `duplicate_check`, `outfit_consolidation`, `picture_match`, `closed_group`, `outfit_slots` or `purchase_match` |
| `POST` | `/owner-questions/triage-outfit-doubts` | `{actor}`: for outfit readings made before readers labeled their doubts. The first reader sorts each open decision question by its text: about which piece it is (stays a decision) or only a fact about a clear piece (the piece is recorded as the reader proposed, joins its outfit, and the question becomes a detail). Answers `{looked, identity, detail, failures, cost_usd}` |
| `GET` | `/owner-questions/{id}` and `…/image` | the image is the sighting cut from its photo |
| `POST` | `/owner-questions/{id}/answer` | `{choice:<index>}` or `{text:"…"}`, and `answered_by`. Words go to the first reader, which turns them into a resolution before anything is written: for a detail question, the possession as it should now read (`amend`). A second answer is a **409** |
| `POST` | `/sightings/record-agreed` | `{actor}`: records every sighting both readers agreed on that still waits on a decision question, and makes that question a detail question. For reviews made before agreed sightings were recorded at once. Answers `{recorded, failures:[{sighting_id, error}]}`; running it again records nothing new |

## Products

A product is a make and model as its listing describes it; any number of
possessions point at one (`product_id`). The page itself is article-store's:
a product keeps its `article_id` and what was read from it, never a copy of
the page.

```json
{"id":"product_000001","brand":"Acme","model_number":"MW-6","name":"Metronome Watch",
 "listing_url":"https://maker.example/mw","listing_source":"manufacturer","listing_publisher":"Acme Instruments",
 "article_id":"article_000007","found_by":"web search \"Acme MW-6\"; the maker's own page",
 "description":"…the listing's own words…","specs":[{"name":"Tempo range","value":"40-304"}],
 "list_price_cents":null,"list_price_currency":"","extracted_by":"claude-opus-5-5","extraction_cost_usd":0.03,
 "created_by":"principal_000001","created_at":…,"updated_at":…,"deleted_at":0,"possession_ids":["possession_000001"]}
```

`listing_source` is `manufacturer`, `retailer`, `marketplace` or `review`:
take the maker's own page, and another only when the maker has none.
`POST /products {actor, brand, model_number, name, listing_url,
listing_source, listing_publisher, found_by, possession_ids, article_id,
description, specs, list_price_cents, list_price_currency}`: without
`article_id` the store saves `listing_url` in article-store (`kind: other`,
tags `product-listing`, `possession-store`; a URL saved before answers the
saved article). Without `description` the first reader reads the article's
text and the whole fetched page (readability drops spec tables) and copies
the description, every spec, and the list price (`null` for "open price"); a
reader that says the page does not list that model is a **400**. A second
product of one brand and model number is a **409**. Finding the listing is
a web search an agent does before calling.

## eBay orders, in budget-store

An eBay order and the charge that paid it are budget-store's: a
`marketplace_order_…` with its `marketplace_order_line_…`s, and the
`transaction_…` it found (budget-store's CONTRACT.md, "Marketplace orders").
A possession points at that transaction by `budget_transaction_id`, checked
against budget-store, and the order line links to the possession there
(`entity_type: possession`). possession-store keeps no copy of the order. The
operator chose this on 2026-11-07: the purchase is the budget-store
transaction, and its chip shows the order.

`POST /marketplace-orders/import-ebay-mail {actor, account, query}` searches
one mailstack account (`query` defaults to `from:ebay@ebay.com
(subject:"order confirmed" OR subject:"order is confirmed")`), skips each
mail whose order budget-store already holds (by `mail_account_id` and
`mail_message_id`), and has the first reader read the rest, four at a time:
order number, seller, total, currency, and each item's full title, item
number, quantity and price. An order number or item number the mail's text
does not hold is a failure, not a guess. Each order is posted to budget-store
oldest mail first, with `ordered_at` the mail's date, `listing_url`
`https://www.ebay.com/itm/<item>` and `extracted_by` the model; budget-store
finds its charge. Counts: `orders`, `lines`, `matched_by_order_number`,
`matched_by_amount_and_date`, `matched_by_no_transaction`,
`already_imported`, `not_an_order`, `order_already_recorded`, `failed`. Mail
is only read.

## Purchase match

`POST /marketplace-order-lines/match {actor, marketplace_order_id}` reads
budget-store's orders and looks at every line still `unreviewed` with no
possession linked from it and not asked about. It shortlists up to 8 live
possessions not already tied to an order (no `budget_transaction_id`, no
`purchase_line_id`, no line linking to them) by the title's words (a word in
the model counts 4, brand 3, name 2, description 1; common listing words such
as "new" and "vintage" count nothing; at least 2 to be shown). Both readers
see the line and the shortlist with each cover picture, pick one (`C1`…) or
none, and say whether they are sure. Both pick the same one and both are
sure: linked — the possession takes `budget_transaction_id` (when the order
has a charge), and `acquired_at`, `acquired_how: bought` and the line's price
where it has none; budget-store links the line to it and marks it
`possession`. Either picks one otherwise: an optional detail question on that
possession's photo, `asked_by: purchase_match`, choices `order_line_link`
(`possession_id`, `marketplace_order_line_id`, `budget_transaction_id`) for
each pick and `order_line_not_this`. Both say none, nothing shares its words,
or the pick has no photo to ask on: the line stays `unreviewed`. Counts:
`matched`, `asked`, `unmatched`, `failed`.

## Photo review

`possession-add-photos -queued-by principal_… [-folder possessions]
[-time-zone America/Los_Angeles] photo.jpg…` keeps each original through
`PUT /possession-store/originals/{sha256}`, adds the photo to photo-store
(upright, with its EXIF date and GPS, at the `source_machine` and
`source_path` that answer names), and queues it, all through the bridge at
`LLM_BRIDGE_GATEWAY_URL` with `LLM_BRIDGE_PRINCIPAL_TOKEN`, so everything is
that principal's. It prints `path, photo id, review id` per photo.

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

## Outfit covers

Every outfit with a photo has a cover, worked out on each read from boxes
already stored, with no model call:

1. **Which photo.** Of the outfit's `photo_ids`, the one where the most of
   its pieces have a box (`possession_photos`, which outfit readings and
   slot links both write); on a tie, the one where those boxes together
   span the most of the photo; then the earliest. The owner's pick
   (`PUT /outfits/{id}/cover`) wins while that photo is one of the outfit's.
   An outfit rebuilt or combined into a new id starts without a pick.
2. **The frame.** The first reader role is asked once per cover photo for
   a tight box around the wearer, from the top of the head to the lowest
   clothing or shoe that shows, told where photo-store's confirmed face of
   the review's `wearer_person_id` is when it has one
   (`outfit_cover_wearer_boxes`, keyed by photo and wearer, with the model,
   tokens and cost). The frame is that box with 3% of its height added on
   every side, in its own shape as long as that is between 1:2.4 and 3:4
   wide over tall in pixels; outside that it is widened or heightened about
   its center to the nearest end, then moved to stay inside the photo. A
   wearer too wide for a 3:4 cut at the photo's full height gets the
   tallest 3:4 cut, centered on them. On 2026-11-07 the read cost
   $0.037 a photo on claude-opus-5-5 (72 photos, $2.64, about 5,600 tokens
   in and 340 out each).

   Until a cover photo's box is read, the frame is the smallest box holding
   the wearer's face and every piece box on that photo, with 8% of its width
   and height added on each side, shaped the same way; with no face and no
   box, the tallest 2:3 cut from the middle. `cover_reason.frame_from` says
   which: `wearer_box` or `piece_boxes`. A new cover photo — a pick, a
   rebuild or a combine — is read when `PUT /outfits/{id}/cover` sets it,
   or in the background, one at a time, once `GET /outfits` lists it with
   the review worker on (a photo the reader fails on waits an hour before
   it is tried again, and the failure is logged);
   `POST /outfit-covers/read-wearer-boxes` reads every one missing.

```json
"cover_photo_id":"photo_000123",
"cover_box":{"left":0.21,"top":0.02,"right":0.62,"bottom":0.98},
"cover_aspect":0.427,
"cover_reason":{"rule":"most_pieces_visible","pieces_visible":3,"pieces_total":4,"wearer_area":0.46,"photos_compared":3,
                "chosen_by":"","chosen_at":0,"ignored_owner_photo_id":"","wearer_face_found":true,"frame_from":"wearer_box"}
```

`rule` is `most_pieces_visible` or `owner_choice` (then `chosen_by` and
`chosen_at` say who and when). `wearer_area` is the share of the photo the
piece boxes span. `ignored_owner_photo_id` names a pick no longer among the
outfit's photos. `wearer_face_found` says whether the frame holds a face.
An outfit with no photo answers all four null. Cutting a full-size original
takes about a second, so cuts are kept in `possession_store.cover_cache_directory` by
photo and frame (any file there may be deleted), `GET /outfits` starts
cutting the listed covers not cut yet in the background, and at most two
are cut at once.

## Full-size pictures

Every box is read on photo-store's thumbnail. Where the photo's owner keeps
its original (found by photo-store's sha256), the readers and every crop use the
original instead, turned to stand as the thumbnail stands: by its EXIF
orientation, checked against the thumbnail itself on a 24×24 gray grid in
every orientation, and refused when no orientation matches. On 2026-11-07
all 122 originals of the outfit selfies matched their thumbnails by EXIF
(orientations 6, 8, 3 and 1), so no box needed turning. A crop for
identification is cut from the full-size picture: tight (8% around the box)
for garments, at most 768 pixels on its longest side; with 30% around the
box and scaled up to at least 768 pixels for watches, jewelry and eyewear.

## Wearable groups as the owner counts them

A garment's group is its category (`garment:top` shirts, `garment:bottom`
pants, `garment:outerwear` jackets), except scarves, belts, gloves and ties,
which are groups of their own (`garment:scarf`); necklaces, pendants and
chains are `jewelry:necklace`; socks are in no group and every pass leaves
them out. A group's `expected_low` and `expected_high` are the owner's own
count of what they own, which identification is told and reports against.

## Looks, slots and identification

Run in this order: set each group's expected range, `POST /outfit-looks/read`,
`POST /outfit-slots/identify`, `POST /outfits/rebuild-from-looks`; then the
owner answers the slot questions.

**Reading looks.** Looks are read from outfit readings' photos and from
photos queued for a look reading (`POST /photo-reviews` with `reading:
look` and `wearer_person_id`: any photo of the owner, selfie or taken by
someone else, group photos included; the review worker leaves these
queued, the look read dates each by photo-store's `taken_at` in the
owner's zone and marks it `done`). For each day with such photos not yet
in a look, up to 12 at a time in time order, the first reader sees that
day's photos (originals where present, with the
owner's face box on each) and lists the looks — one per set of clothes, every
photo in exactly one — and each piece worn as a slot: what and where ("watch,
left wrist", "ring, right ring finger", "jacket"), its group, name,
description, kind fields and a box on each photo that shows it, and says
per look whether the owner has nothing on the upper body
(`wearer_has_no_top`: no shirt, top, jacket, coat or dress). The second
reader sees the same photos and that list and writes the final one: each
slot kept (`agreement: both_agree`), corrected (`second_reader_changed`) or
added (`second_reader_added`); a slot it removes is named in `left`. A day
whose final list leaves a photo out of every look, or puts one in two, is a
failure and nothing is written for it; it is read again next time. Two
calls a day.

**Identification.** Group by group, every slot not yet identified is cut
from its largest box, and every recorded owned or unconfirmed member of the
group from its cover. Each reader on its own sorts the pictures into
individual pieces, told the owner's expected range; slots are `S` pictures,
recorded members `R`. A group whose expected high is at most 12 is sorted in
calls of up to 80 pictures; others in calls of up to 30. A group too big for
one call is sorted batch by batch (similar names together). Then one picture
of every piece found so far — a recorded one where the piece has one, and
every piece of one picture — is sorted across the batches: in one call when
they fit; otherwise cut into chunks of half a call and every two chunks
sorted together, so every two pieces are seen side by side once (83
pictures in batches of 30 that find nothing alike: 3 batch calls, then
6 chunks of 15 and 15 calls, per reader). Across chunks, two pictures are
one piece when every call that showed both put them together, and pieces
join only when no call kept any two of their pictures apart. The first
reader's pieces are the best guess:

- a piece holding recorded members becomes the one kept by `keeperOf`
  (owned before unconfirmed, then photos, then age); another recorded member
  in it is merged into that one, undoably, when it is unconfirmed or the
  second reader also put them together, and otherwise stays a candidate;
- a piece with no recorded member becomes a new `unconfirmed` possession,
  except in a closed group, where its slots are ranked against the owned
  members only.

A slot is linked only to a piece a reader put it with (the first reader's
piece, or the second reader's piece overlapping it); a maybe (`uncertain`)
alone, or a closed group's member listed only as a choice, is not a pick.
A slot no reader picked a piece for stays unlinked, `decided_by: no_pick`,
with its candidates, proposed sightings and the same question, whose answer
links it. Identification first unlinks so each earlier best guess whose
candidates all had the same likelihood (no reader's pick; a closed group's
first member by id took it), counted as `unlinked_unpicked`.

Each slot's candidates are scored: 0.5 when the first reader put it in the
piece, plus 0.5 × the overlap (Jaccard) of the second reader's piece for it
with that piece, plus 0.15 for each reader that named the piece as one it
could be with; scores are scaled to sum to 1. This is a combined score, not
a measured probability. An open group keeps the top five; a closed group
keeps every owned member. The slot is linked to the top one (`decided_by:
best_guess`): a sighting on each of its photos (`agreement: best_guess`),
each photo linked to the possession, and one optional detail question
(`asked_by: outfit_slots`) whose choices are `slot_link` to each candidate,
`slot_new` (a piece not recorded yet; not in a closed group) and
`slot_not_mine`. A `slot_link` to a piece merged since the question was
asked links the piece it was merged into. Answering relinks the slot, its
sightings and photos, and keeps its look's outfit right: an outfit worn only
as that look holds the look's pieces; in one shared with other looks the
look stays while its main pieces still match theirs (below), and the outfit
holds the union of its looks' pieces; otherwise the look moves, with its day
and readings, to the other outfit whose looks it matches best, or a new one.

**Pulling a group toward its range.** After the slots, every group with an
expected high that holds more live owned and unconfirmed pieces than that
gets one more round: both readers see one picture of each piece (the
largest slot crop linked to it, else its cover), sorted as above, told the
owner's range, how many pieces the sorting made and that it split pieces
that are one. Every set of pieces both readers put together (the same
piece in both sortings) is merged, undoably, into the one `keeperOf` keeps
— owned before unconfirmed, then photos, then age — so two owned pieces
merge only when both readers agree. One round, whether or not it reaches
the range; the group's `pull_in` says `before`, `shown` (pieces with a
picture), `merged` and `after`. It runs on groups with no new slots too, so
running identification again with nothing new to identify refines the
groups still over their range. A merge repoints the slots linked to the
duplicate (`slots_repointed`), so looks and outfits follow it.

**Not an outfit.** The owner chose on 2026-11-07 that a photo of them with
no top on is a body check, never an outfit. A look is not an outfit when
the second reader said `wearer_has_no_top`, or when none of its slots is a
`garment:top`, `garment:one_piece` or `garment:outerwear` (a slot the owner
answered "not mine" does not count). The rebuild keeps such a look, sets
its `not_an_outfit` to the reason, takes it out of any outfit it is in (its
day's wearing goes when no other look of that outfit was that day, and
pieces only it held go), and takes its readings off their outfits. An
outfit left with no look and no reading is removed, `merged_into_id` null.
A look that is an outfit again (a slot answer) loses its mark at the next
rebuild.

**Rebuilding outfits.** An outfit is known by its main pieces: the pieces
of its looks' `garment:top`, `garment:bottom`, `garment:one_piece` and
`garment:outerwear` slots. Accessories — jewelry, watches, belts, scarves,
headwear, bags, eyewear, footwear and every other group — vary from day to
day and photo to photo, so they are not part of its identity. A look is an
outfit when no main piece visible in both differs — in each main group
both show, one's pieces are all among the other's; a piece out of view
never counts against — and they share a top, one-piece or outerwear, or
two main pieces, on any day. A look showing only one main piece (a bottom,
say) also matches on it an outfit worn the same day (a look of that day is
already in it).

Every rebuild places **every** look again, in day order and within a day
those showing more main pieces first: each joins the matching outfit
sharing the most main pieces, then with more looks, then the earlier, or
starts one. Each resulting outfit keeps the live outfit most of its looks
were in (an earlier group's pick aside), or is new (`created_by` the
rebuild), so outfits that match under the rules join, wherever they came
from. The outfit holds the union of its looks' pieces (each look keeps its
own slots), is worn on its looks' days, and takes the owner's marks of the
outfits its looks came from: favorite when any was, the union of tags, and
the owner's cover pick when it has none and the pick's photo is one of its
looks' photos. A look whose every photo is a copy — photo-store's file name
begins `Screenshot`, or the photo is tagged `screenshot` — joins its outfit
but adds no day of its own. A look with nothing linked, or not an outfit,
leaves its outfit. The looks' readings point at their look's outfit (none
for those). An outfit left with no look and no reading is deleted, with
`merged_into_id` naming the outfit that took most of its looks (null when
none did). One transaction, one `outfit_rebuild`; `moved.outfits_before`
holds every existing outfit it changed as it was (pieces, days, favorite,
tags, cover pick, deleted), and undo restores each whole — so an owner's
mark made after the rebuild is undone with it. Covers need nothing more: a
rebuilt outfit's photos are its looks' readings.
