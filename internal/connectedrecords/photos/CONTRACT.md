# photo-store — API contract

photo-store runs inside llm-bridge-server, at `/photos/` with the paths below, and keeps its records in Postgres (schema `photos`, role `photos_module`). It answers only a request carrying the caller the server verified; one with none is 401. What is known about each owner's own photos and videos. The originals stay on the machine they were found on (`source_machine`, `source_path`); this store keeps the facts, and file-store, in the same process, keeps one thumbnail file for each photo (`photos.thumbnail_file_id`, never in an answer).

## Owners

Every record has an owner, `owner_principal_id` (principal-store's `principal_000001`), the person who keeps it.

- `POST /imports` upserts among the caller's own photos, by `sha256`: a new photo is owned by the caller, and a caller that acts as no principal cannot import (403). `sha256` is unique per owner, so two people may each keep a photo of the same original, each with a thumbnail file of its own; a purge by one never touches the other's. A purged photo makes later imports skip that `sha256` for its owner only.
- Revisions, sightings, face scans, faces and links (`photo_people`) take their photo's owner; a face group takes its faces' owner; members and guesses take their group's. The database holds every child to its parent's owner, so a face of one owner's photo can never sit in another owner's group or be named on another owner's photo.
- A caller reaches only its own records. A record of another owner answers 404, as a missing one does, on reads and writes alike; a face of another owner named in a body is 400, as a missing face is. Lists and `/health` counts hold only the caller's own. An administrator, or the server's own service, reaches every owner's, and `?owner_principal_id=` narrows any read to one owner; anyone else may name only itself there (403 otherwise). For an administrator `GET /photos?sha256=` may answer one photo per owner.
- `POST /face-groups` needs every face of one owner: an administrator naming two owners' faces is 400.
- A `person_id` in `POST /photo-people`, a face group's naming decision, or a guess is checked in the **photo's or group's owner's** people-store records: a person that owner does not keep is 400, whoever asks, and a people-store that cannot answer is 502. A guess's `person_id` `""` still means nobody in people-store. Imports never check people.
- The vocabularies (`/date-sources`, `/photo-person-statuses`, `/face-group-statuses`) answer any caller.

## `photos`

| Field | Meaning |
|---|---|
| `id` | `photo_000001`; reference-store's type `photo` |
| `owner_principal_id` | who keeps it |
| `sha256` | of the original file; an import finds a photo again by it |
| `source_machine`, `source_path` | where the original is |
| `folder` | the collection it came from (`Pictures/Camera`) |
| `media_type` | `image/jpeg`, `video/mp4`… |
| `taken_at` | epoch seconds; 0 only with `date_source` `unknown` |
| `date_source` | `GET /date-sources`: `exif` (camera), `filename`, `mtime` (weak: copies reset it), `message` (when it was sent in a chat: taken then or before), `upload` (when it was posted to a site such as Facebook: taken then or before), `google_photos` (Google Photos' own record of when it was taken, from a Takeout sidecar: usually the camera's time, weaker than `exif`), `unknown` |
| `latitude`, `longitude` | both or neither; null when the file has no GPS |
| `width`, `height` | of the original |
| `description` | markdown; `[text](ref:<id>)` links name people and events |
| `described_by` | who wrote the description; required with one |
| `tags` | JSON array of strings |
| `deleted_at`, `purged_at` | epoch seconds; 0 = not. A purged photo is also deleted |

## Routes

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{photos, described, with_gps, deleted, revisions}}`, counted in the caller's reach |
| `GET` | `/date-sources` | `["exif","filename","mtime","message","upload","google_photos","unknown"]` |
| `GET` | `/tags` | every tag on a live photo in reach with how many such photos carry it, most used first: `[{tag, photos}]` |
| `POST` | `/face-scans` | `{photo_id, model, faces:[{box:{left,top,right,bottom}, detection_score, embedding_float32_base64}]}`: what one face model found on one photo's thumbnail, all or nothing; an empty `faces` records that it found none. Boxes are fractions of the thumbnail, up to 0.5 past an edge. The embedding is stored normalized to length 1. A photo already scanned by the model is a **409**, so a face id keeps meaning one face, unless the body sets `"replaces": true`: then the faces of the earlier scan are retired and the new ones get new ids. **201** with the faces |
| `GET` | `/face-scans/pending` | `?model=` (required) `&limit=`: ids of live, unpurged images that model has not scanned, oldest first |
| `GET` | `/faces` | faces on live photos in id order, retired faces left out: `?photo_id=`, `?model=`, `?after=<face id>` to page, `?include_embeddings=true`, `?limit=` (default and max 5000) |
| `GET` | `/photos/{id}/faces` | the photo's faces, no embeddings |
| `GET` | `/faces/{id}/image` | the face cut from its photo's thumbnail with a third of its size around it, a JPEG; a purged photo's is a **410** |
| `GET` | `/photo-person-statuses` | `["suggested","confirmed","rejected"]` |
| `GET` | `/photo-people` | who is in which live photo: `?photo_id=`, `?person_id=`, `?status=`, `?limit=`, `?offset=` |
| `POST` | `/photo-people` | `{photo_id, person_id, face_id, status, confidence, evidence, proposed_by, decided_by}`: `person_id` is people-store's (`person_000001`), one the photo's owner keeps; `status` is `suggested` or `confirmed`, which needs `decided_by`; `face_id` must be on the photo. For a pair already linked it only moves a suggestion upward: a confirmed or **rejected** link is answered as it stands (200), so no matcher undoes a person's rejection. **201** when created |
| `PATCH` | `/photo-people/{id}` | any of `status`, `decided_by`, `face_id`, `evidence`; a status change needs `decided_by`. Every change keeps the old row in `photo_person_revisions` |
| `GET` | `/people-counts` | `[{person_id, confirmed, suggested}]` over live photos, rejected links left out, most confirmed first |
| `GET` | `/face-group-statuses` | `["open","named","not_known","mixed"]` |
| `GET` | `/face-groups` | groups of faces a grouper found to be one person, the most photos first: `?status=`, `?person_id=`, `?limit=` (default 100), `?offset=`. Each has `face_count`, `photo_count`, `first_taken_at`, `last_taken_at`, up to 12 `sample_face_ids` (one per photo, spread evenly from the most typical to the least, so an odd face shows) and its `guesses`, newest first |
| `POST` | `/face-groups` | `{model, made_by, members:[{face_id, similarity}]}`: a new open group. A face that is retired, on a deleted photo or found by another model is a 400; a face already in a group is a **409**. **201** with the group |
| `GET` | `/face-groups/{id}` | one group with every face in `members` (`face_id`, `photo_id`, `similarity` to its average face, `taken_at`, `folder`) |
| `POST` | `/face-groups/{id}/members` | `{members:[{face_id, similarity}]}`: faces that belong to an **open** group; a decided group is a 409 |
| `POST` | `/face-groups/{id}/guesses` | `{person_id, confidence, evidence, proposed_by}`: who a model or the context thinks it is; `person_id` `""` means nobody in people-store. Kept as written. **201** |
| `POST` | `/face-groups/{id}/decision` | `{status, person_id, decided_by, excluded_face_ids}`: a person saying who it is. `excluded_face_ids` leave the group first, free to be grouped again. `named` (needs `person_id`) confirms every remaining face as the person through the rules of `POST /photo-people` (a rejection holds, a confirmed link stays) and rejects suggestions of anyone else on those faces; `not_known` is someone not to name; `mixed` is more than one person. An open group can be decided, a `not_known` or `mixed` one can still be named, and a `named` one can only be named as someone else with nothing left out, which rejects the first person's links on its faces (`previous_person_rejected`); anything else on a named group is a **409**. Answers `{group, confirmed, already_confirmed, kept_rejected, others_rejected, excluded, previous_person_rejected}` |
| `GET` | `/photos` | live photos, oldest taken first. `?from=` (inclusive) `&to=` (exclusive) on `taken_at`, `?folder=`, `?q=` (every word, as a substring in any case, in the description or in one tag), `?tag=`, `?person=` (a `person_…` confirmed in it), `?described=true|false`, `?has_gps=true`, `?sha256=` (the original file's hash: the owner's one photo with it, or none), `?include_deleted=true`, `?exclude_tag=` (repeatable: leave out photos carrying it), `?limit=` (default 100, max 1000), `?offset=` |
| `GET` | `/taken-activity` | live photos counted per span of `taken_at`, earliest first: `[{start, photos, sample_photo_ids}]`. `?interval=` `day`, `week` (from Monday), `month` or `year`; `?time_zone=` an IANA zone, required, in which spans start (`start` is epoch seconds); `?samples=` 0–24 ids per span, spread evenly through it; the same `from`, `to`, `folder`, `q`, `tag`, `person`, `described`, `has_gps` and `exclude_tag` as `/photos`. Photos with no `taken_at` are not counted |
| `POST` | `/imports` | `{"photos":[ImportPhoto…]}` with `thumbnail_jpeg` as base64. Upsert by `sha256` among the caller's own photos: new is created, owned by the caller, known with the same facts is skipped, known with other facts is updated after a revision. **Description and tags are never touched by an import.** One bad photo is a 400 naming its index and writes nothing. A known photo whose thumbnail arrives with other bytes gets a new thumbnail file, has its faces retired and its scans forgotten, so it is scanned again; `faces_retired` in the answer counts them. The same bytes write nothing. If the rows fail, every thumbnail file the import kept is destroyed |
| `GET` | `/photos/{id}` | deleted is 404 unless `?include_deleted=true` |
| `PATCH` | `/photos/{id}` | any of `description`, `described_by`, `tags`, `taken_at`, `date_source`; any other key or `null` is a 400 |
| `DELETE` | `/photos/{id}` | soft, **204** |
| `POST` | `/photos/{id}/restore` | a purged photo is a **409** |
| `POST` | `/photos/{id}/purge` | removes the photo for good, for a picture that must not be kept (an ID, a card, a document): destroys its thumbnail file, blanks its description in the row and every revision, marks it deleted and purged, and makes every later import skip it. The row stays so a citation still resolves. Again is a no-op. Answers the photo |
| `GET` | `/photos/{id}/revisions` | newest first, each the whole row as it was |
| `GET` | `/photos/{id}/thumbnail` | the JPEG, read from file-store; served for a deleted photo too, so a citation keeps drawing; a purged photo is a **410**; a photo that is not purged and has no file, or whose file file-store cannot open, is a **500** naming the photo |

## Moving a standalone store in

`ImportStandaloneDatabase(ctx, db, thumbnails, sqlitePath, thumbnailsDirectory, owner)` copies a standalone photo-store's SQLite file, every id kept and every row owned by `owner`, and carries each unpurged photo's `<thumbnailsDirectory>/<sha256>.jpg` into file-store first. A missing thumbnail stops it, naming the photo, before anything is copied; a failure of the copy destroys every thumbnail it carried. A file with no sightings has them filled from the revisions and the photos, as the standalone store did. Each table numbers on after the highest id copied. It refuses a module that already holds rows.

## What changed in the move

- The store no longer listens on `127.0.0.1:8329` and has no settings of its own: no `PHOTO_STORE_ADDR`, no `PHOTO_STORE_DATA_DIR`, no `GET /settings`. Thumbnails are file-store's files, not `thumbnails/<sha256>.jpg`.
- Owners, as above, and the people-store check of `person_id`: the standalone store checked only the id's shape.
- `q` was SQLite `LIKE`, which folds only ASCII letters, over the description and the tags' JSON text; it is now `ILIKE`, which folds every letter, over the description and each tag on its own, so a word no longer matches the JSON's quotes and commas or runs across two tags.
- Ties in `/tags`, `/people-counts` and `/sightings` sort by byte order, as SQLite did.
- A revision in `GET /photos/{id}/revisions` carries the photo's `owner_principal_id`; a revision keeps no thumbnail file id.

## Importing

These commands are HTTP clients of the routes above. Their `-store-url` still defaults to the standalone address, `http://127.0.0.1:8329`.

`photo-import -dir <dir>` posts a directory prepared on the machine that
holds the originals: `index.jsonl`, one line per photo with the fields of
`ImportPhoto` and `thumbnail` naming its JPEG inside the directory, and the
thumbnails. Batches of 50; running it again changes nothing that did not change.

`photo-import-messages [-max-members 8] [-discord-archive <dir>] [-dry-run]`
imports the photos people sent in the owner's chats, read through multichat
(`MULTICHAT_URL`, `MULTICHAT_TOKEN`): every image in the message log of a room
with at most `-max-members` members, never a Discord server channel, plus with
`-discord-archive` the image attachments `multichat-fetch-discord-history`
saved. Each photo's `source_machine` is `multichat`, its `source_path` the
`message_…` ref it came in (a chip), its `folder` `messages/<platform>/<room>`
or `discord/<server>/<channel>`, and its date when it was sent
(`date_source` `message`). Thumbnails are made in Go (package `thumbnail`, public so another repo's importer makes the same ones:
JPEG, PNG, GIF, WebP; longest side 1024, quality 80).
