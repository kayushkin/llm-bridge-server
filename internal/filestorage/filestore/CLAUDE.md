# About file-store

## What it owns

`127.0.0.1:8317`, unit `file-store.service`. Uploaded bytes: a ticket's attachment, a mail's attachment, a chat's image. A **file** is one upload — these bytes, under this name, put here by this service (`owner_service`) for this thing of its own (`owner_ref`) — with ids `file_000001`. The record is in SQLite (`~/.config/file-store/file-store.db`); the bytes are on disk under `blobs/`, named by their SHA-256, so identical uploads share one blob and a blob lives as long as any row, deleted or not, names it. It owns nothing else: not which card or message a file hangs on, and not who may read it. `CONTRACT.md` is the route table and `README.md` the reasoning.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps only a few words for this repo in its map.

# How it works

## It decides nothing about who may read a file

A file is readable exactly when the service that owns the thing it hangs on says so, and that service is the caller. kanban-store is the first: its routes sit under `/api/cards/{id}/attachments`, so its gate has held the caller to the card, it checks the file hangs on **that** card, and only then fetches the bytes here with the service token. **Never add a route here that judges a caller, and never add one to a caller that takes a file id alone** — the second turns "who may view this card?" into "who can guess an id?". A new consumer (mail, chat) does the same: its own table of which file hangs on what, its own access check, then this store for the bytes.

## An upload is somebody else's bytes

A person's upload shown from the dashboard's origin would run with the dashboard's cookies. So `GET /files/{id}/content` always sends `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox; default-src 'none'`, and serves `application/octet-stream` as an attachment **unless** the caller asks `?inline=true` and the file's *declared* type is in the `inline_content_types` setting. `ValidateInlineContentTypes` refuses a type a browser runs (`text/html`, `image/svg+xml`, XML, JavaScript) on `PUT /settings` **and at start**, since an environment seed never passes through `PUT`. A caller that relays a download must pass these headers on unchanged; kanban-store does. The filename is never used as a path here and is still refused unless it is a plain name, because the next program to save the file will use it as one.

## An id is given out once

Ids come from the one-row `file_sequence` counter, never from `MAX(seq) + 1`: a purge removes the row, and the next upload would be handed the purged file's id, so a reference left in another store would open somebody else's upload. That happened on the first live run — a ticket's attachment came back as the `file_000001` the deploy's smoke probe had just used and purged — and `TestAPurgedFilesIdIsNeverGivenOutAgain` pins it. A failed insert burns an id; a gap is harmless.

## Deleting, and the blob lock

`DELETE` is reversible (the row and the bytes stay; `POST /files/{id}/restore`). `?hard=true` destroys the row, live or deleted, and the blob when no other row names its hash. `Store.blobMutex` makes "is this blob still named?" and the unlink one step against an upload of the same bytes finishing in between; it also serializes id minting. Remove it and `TestUploadsAndPurgesOfTheSameBytesNeverLoseAFile` fails at once. A caller that owns files for a thing must purge them when the thing is purged, **deleted ones included** (`GET /files?owner_service=…&owner_ref=…&include_deleted=true`): kanban-store first went by its own rows and left removed attachments here for good.

# Access and operations

## Who may call it

Every route but `GET /health` needs `X-File-Store-Service-Token`; a bare or wrong call is **401**. **Whoever holds the token reads every file**, so it goes only to services that check access themselves. It lives in `~/.config/file-store-tokens.env` (mode 600; `deploy.sh` refuses any other mode) and reaches this unit and each caller through a host-local drop-in (`file-store.service.d/service-token.conf`, `kanban-store.service.d/file-store.conf`). ⚠️ **Not** `~/.config/principal-gating-tokens.env`: llm-bridge-server reads that file and every agent session inherits its variables. Never put the token in a tracked unit, and never give it to a harness. The service binds to loopback and `deploy.sh` fails if it is listening anywhere else; **never proxy it from dash, nginx or the bridge gateway**.

## Settings

Configuration is declared in `settings.go` and read through `llm-bridge/servicesettings`; nothing calls `os.Getenv`, and the process refuses to start on a `FILE_STORE_` variable nothing declares. `GET /settings` describes it. `maximum_file_bytes` (25 MiB) and `inline_content_types` are stored in this database and changed with `PUT /settings/{key}`, no restart; for those two **the environment variable only seeds the first start** and the stored row decides after. `GET /limits` serves both to an upload form.

# Working in this repo

## Build, test and deploy

`go test ./...`; `./deploy.sh` from `main` only — it tests, installs the unit and binary, then proves a file goes in, comes back byte for byte and is purged, and checks the bind. `filestore.Start` is the one startup path: `main` calls it with the process environment and every test with a map, because the first deploy panicked in a `main` no test ran. It links `../llm-bridge` through a `replace`, so the deploy gate also wants that clone clean and pushed. `./generate-ts.sh` renders `file.go` to `ts/model.ts` as `@kayushkin/file-store-types`, which kanban-store's generated types import for an attachment's `file`. The GitHub repo is **private**; its siblings are public, and making it public is the operator's call.
