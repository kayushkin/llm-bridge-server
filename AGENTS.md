# About possession-store

## What it owns

An inventory of the things the operator owns, on `127.0.0.1:8341` with no auth: dash is the front door. `CONTRACT.md` is the route table and the source of truth for every shape.

It owns possessions and their kinds (garments, computers, computer parts, audio devices, musical instruments, vehicles: a table each, with its own id prefix), outfits, purchases and their lines, and the photo review (reviews, model readings, sightings, owner questions). It does not own photos (photo-store), places (location-store), machines (harness-store) or what kind of instrument something is (music-practice-store): it stores their ids and checks each one against the owning store before writing it. Net worth is not here yet; it waits on budget-store.

A new kind is a new table and a new entry in `KindTypes` (`records.go`), never a column holding the kind's name. Add one when enough possessions share facts worth searching on; until then a possession with no kind is complete.

# How it works

## Photo review

Two model-store roles read each queued photo: `possession-photo-reader` (claude-opus-5-5) and `possession-photo-second-reader` (gpt-6-astra), chosen by the operator on 2026-11-06 so that two makers' models must agree. Each holds one model and no fallback. Calls go through llm-bridge-server's `POST /oneshot` with the service token from the unit's host-local drop-in; costs come from model-store list prices.

Round one: each reader lists what it sees on its own. Round two: each sees both lists and the inventory and writes a final list. `recordReconciliation` (`review.go`) pairs the two lists; what agrees with no doubt is recorded at once, the rest becomes an owner question whose choices carry their resolution. An answer in words goes to the first reader, which turns it into a resolution. Settling a sighting settles every later one waiting on it (`same_as_other_sighting`).

The vocabularies in `vocabulary.go` feed both validation and the readers' prompt (`kindGuide`): change them there and nowhere else. A reader's field that does not fit is dropped and named in `dropped_fields`, never stored.

## Bringing photos in

`possession-add-photos -queued-by principal_… [-time-zone America/Los_Angeles] photo…` copies each original to `<data dir>/originals/<sha256>.<ext>`, adds it to photo-store upright with its EXIF date and GPS, and queues it. It reads JPEG, PNG and WebP, not HEIC. `source_machine` is harness-store's one machine with transport `local`. The readers see the original scaled to 2048 pixels when it is in `originals/`, and photo-store's 1024-pixel thumbnail otherwise.

# Working in this repo

## Build, test and deploy

`go test ./...` runs everything, with scripted readers in `store_test.go`; `server_test.go` builds the whole mux, so a route collision fails a test rather than the deploy. `./deploy.sh` (through deploy-gate) tests, builds `possession-store` and `possession-add-photos` into `~/bin`, installs the user unit and checks `/health` and the loopback bind. The service refuses to start with the review worker on and no `LLMBRIDGE_SERVICE_TOKEN`.

## Nothing personal in the repo

The repo is on GitHub and may become public. Tests use made-up ids, places and coordinates. What the operator owns lives only in the database.
