# About possession-store

## What it owns

An inventory of the things the operator owns, on `127.0.0.1:8341` with no auth: dash is the front door. `CONTRACT.md` is the route table and the source of truth for every shape.

It owns possessions and their kinds (twenty: garments, computers, computer parts, audio devices, musical instruments, vehicles, furniture, fitness equipment, sports gear, tabletop games, media, collectibles, art and decor, home textiles, appliances, personal care, bags and cases, electronic accessories, household tools, music accessories — a table each, with its own id prefix; `KindTypes` in `records.go` is the list), outfits, purchases and their lines, and the photo review (reviews, model readings, sightings, owner questions). It does not own photos (photo-store), places (location-store), machines (harness-store) or what kind of instrument something is (music-practice-store): it stores their ids and checks each one against the owning store before writing it. Net worth is not here yet; it waits on budget-store.

A new kind is a new table and a new entry in `KindTypes` (`records.go`), never a column holding the kind's name. Add one when enough possessions share facts worth searching on; until then a possession with no kind is complete. The fourteen after vehicles came on 2026-11-06, when the first 52 photos left 223 of 288 possessions with no kind; `POST /possessions/classify-kindless` sorted the ones already recorded. A room (`Rooms`) is on the possession itself, never on a kind.

# How it works

## Photo review

Two model-store roles read each queued photo: `possession-photo-reader` (claude-opus-5-5) and `possession-photo-second-reader` (gpt-6-astra), chosen by the operator on 2026-11-06 so that two makers' models must agree. Each holds one model and no fallback. Calls go through llm-bridge-server's `POST /oneshot` with the service token from the unit's host-local drop-in; costs come from model-store list prices.

Round one: each reader lists what it sees on its own. Round two: each sees both lists and the inventory and writes a final list. `recordReconciliation` (`review.go`) pairs the two lists. What both agree on is recorded at once; a doubt either raised about it becomes an optional **detail** question, whose answer in words amends the possession. The rest waits on a **decision** question whose choices carry their resolution. An answer in words goes to the first reader, which turns it into a resolution. The operator chose on 2026-11-06 that agreement records without waiting: holding agreed things for a detail made 62% of sightings into questions. Settling a sighting settles every later one waiting on it (`same_as_other_sighting`).

The vocabularies in `vocabulary.go` feed both validation and the readers' prompt (`kindGuide`): change them there and nowhere else. A reader's field that does not fit is dropped and named in `dropped_fields`, never stored.

**Outfit reading** (`reading: outfit`, `outfit_reading.go`): one reader, the first role, reads a photo of the wearer (`wearer_person_id`, located by photo-store's confirmed face) against every wearable already recorded. The operator chose one reader for outfits on 2026-11-06; it costs about $0.09 a selfie. New pieces start `unconfirmed` with a "do you still have it?" detail question, because a photo shows the wearer had a thing then, not now. A reading's pieces make an outfit or find the live one with exactly the same pieces, and add a wearing on the photo's day in `POSSESSION_STORE_OWNER_TIME_ZONE`. Wearable kinds (`WearableKinds`): garment, jewelry, watch, eyewear, bag_or_case; an outfit holds nothing else.

## Bringing photos in

`possession-add-photos -queued-by principal_… [-time-zone America/Los_Angeles] photo…` copies each original to `<data dir>/originals/<sha256>.<ext>`, adds it to photo-store upright with its EXIF date and GPS, and queues it. It reads JPEG, PNG and WebP, not HEIC. `source_machine` is harness-store's one machine with transport `local`. The readers see the original scaled to 2048 pixels when it is in `originals/`, and photo-store's 1024-pixel thumbnail otherwise.

# Working in this repo

## Build, test and deploy

`go test ./...` runs everything, with scripted readers in `store_test.go`; `server_test.go` builds the whole mux, so a route collision fails a test rather than the deploy. `./deploy.sh` (through deploy-gate) tests, builds `possession-store` and `possession-add-photos` into `~/bin`, installs the user unit and checks `/health` and the loopback bind. The service refuses to start with the review worker on and no `LLMBRIDGE_SERVICE_TOKEN`.

## Nothing personal in the repo

The repo is on GitHub and may become public. Tests use made-up ids, places and coordinates. What the operator owns lives only in the database.
