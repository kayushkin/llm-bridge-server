# About reference-store

## What it owns

`127.0.0.1:8343`. **What an id written in prose names, who may see it, and where its page is.** Every chat and page draws ids as chips: `prediction_000012`, a noteboard uuid, `repo:dash`, `dash@8487d32`, a session id. This store holds the one list of those reference types (`types.go`: id patterns, the owning service's fetch route, the field that names a record, a few summary fields with format hints, the page link, a glyph, and how access is decided) and looks ids up in the stores that own them. It keeps no database: the list is code, and records stay in their stores.

There is one reference-store per llm-bridge-server, and an id means the record on the server holding the text it was written in. A record on another server is written `<id>@<server_id>` and looked up through that server's connection proxy, by that server's reference-store, under that server's access rules.

# How it works

## Routes

- `GET /types`, `GET /types/{type}`: the list, open to any caller. kanban-store, journal-store and prediction-store read their type vocabulary from here; chat surfaces read the id patterns to spot ids locally.
- `GET /vocabulary`: the summary formats, the access rules, and the grant relation and resource types.
- `POST /resolve {"ids":[…]}` (at most 128): for each id, every record it names. A match carries the store's answer unchanged in `data`, with `label` (the row's label field) and `page_url` (the row's page template filled in) beside it; a record the caller may not see comes back `{"type","service","refused":true}` with nothing else. An id nothing holds is `[]`. A store that could not answer goes in `errors` and is not cached.
- `GET /attachments/{id}`: a picture of the record (the row's `attachment` route on the owning service: a photo's thumbnail, a possession's or outfit's image), for the caller, at most 10 MiB and only an `image/…`; 404 when the id names no one record the caller may see with a picture. A chat link's reply attaches these.
- `GET /search?q=`: the @ picker. Each mentionable type the caller may see whole is searched and its answer passed through.

Every route but `/health`, `/settings`, `/types` and `/vocabulary` needs a caller: `X-Principal-Id`, which only llm-bridge-server's gateway sets, or `X-Reference-Store-Service-Token` for an internal service acting for nobody (grant-store checking that a record exists). Neither is a 401. **The service trusts `X-Principal-Id`, so it must stay on loopback and be reached through the gateway.**

## Access

A row's `access` says who decides:
- `grant`: an administrator (principal-store's `is_administrator`) sees everything. Anyone else needs grant-store's `can_see_references` on `reference_type <type>`, on `reference_type *`, or on `reference <id>` for one record. Holding none sees none, and a disabled principal sees nothing.
- `owner`: the owning service decides, asked as the caller (`X-Principal-Id` with the service's credential). A 403 is a refusal. llm-bridge-server answers 404 for a session the caller may not see, so such a session reads as unknown rather than refused.

Principal and grant answers are cached `access_cache_seconds` (30). A store that cannot answer is an error, never "sees nothing".

## Caching

A store's answer for (type, id) is reused for `record_cache_seconds` (60), and a 404 for `miss_cache_seconds` (15); an owner-decided type is cached per caller. Ids are spotted locally by the clients from the patterns, so only an id that matches a pattern ever reaches this service.

## Adding a type

One row in `types.go`. Configure deployed services in `REFERENCE_STORE_SERVICE_URLS`. Unconfigured services remain in the catalog; lookups and searches return explicit errors without sending a request. An unconfigured page site leaves the page link empty. The supplied unit covers the full catalog, checked by `TestTheUnitAddressesEveryService`. `TestNoTwoTypesClaimOneID` holds that no two patterns claim one id. Check each summary field and the page against a real record before you ship the row: a missing field shows nothing, and a page placeholder the record lacks leaves no link.

A type with no page of its own leaves `page` empty; the chip then opens the generic record page that shows any record.

# Access and operations

## Unit, secrets and deploy

User unit `reference-store.service`. `deploy.sh` runs the deploy gate and the tests, then writes `reference-store.service.d/tokens.conf` (mode 600) from the files that own each token: its own service token and llm-bridge-server's from `~/.config/principal-gating-tokens.env` (it makes its own once, there, where llm-bridge-server and grant-store read it), and auth-store's bearer from the installed `auth-store.service`. Credentials a store needs come from auth-store by provider (`REFERENCE_STORE_AUTH_STORE_PROVIDERS`, `service:provider`).

# Working in this repo

## Tests

`go test ./...` runs against fake stores. A check against the live stores is a trial on a spare port with `POST /resolve` for one id of each type; run it after any change to a row.
