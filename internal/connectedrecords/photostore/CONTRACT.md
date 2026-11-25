# photo-store — API contract

`127.0.0.1:8329`, loopback only, no auth: dash is the front door. What is known
about the user's own photos and videos. The originals stay on the machine they
were found on (`source_machine`, `source_path`); this store keeps a thumbnail
of each (`<data dir>/thumbnails/<sha256>.jpg`) and its facts. SQLite at
`<data dir>/photo-store.db` (default `~/.config/photo-store`, directory 0700,
file 0600).

## Settings

| Variable | Default | What it is |
|---|---|---|
| `PHOTO_STORE_ADDR` | `127.0.0.1:8329` | listen address |
| `PHOTO_STORE_DATA_DIR` | `~/.config/photo-store` | holds `photo-store.db` and `thumbnails/` |

## `photos`

| Field | Meaning |
|---|---|
| `id` | `photo_000001`; kanban-store's registry type `photo` |
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
| `GET` | `/health` | `{status, counts:{photos, described, with_gps, deleted, revisions}}` |
| `GET` | `/settings` | the declared settings |
| `GET` | `/date-sources` | `["exif","filename","mtime","message","upload","google_photos","unknown"]` |
| `GET` | `/tags` | every tag on a live photo with how many live photos carry it, most used first: `[{tag, photos}]` |
| `POST` | `/face-scans` | `{photo_id, model, faces:[{box:{left,top,right,bottom}, detection_score, embedding_float32_base64}]}`: what one face model found on one photo's thumbnail, all or nothing; an empty `faces` records that it found none. Boxes are fractions of the thumbnail, up to 0.5 past an edge. The embedding is stored normalized to length 1. A photo already scanned by the model is a **409**, so a face id keeps meaning one face, unless the body sets `"replaces": true`: then the faces of the earlier scan are retired and the new ones get new ids. **201** with the faces |
| `GET` | `/face-scans/pending` | `?model=` (required) `&limit=`: ids of live, unpurged images that model has not scanned, oldest first |
| `GET` | `/faces` | faces on live photos in id order, retired faces left out: `?photo_id=`, `?model=`, `?after=<face id>` to page, `?include_embeddings=true`, `?limit=` (default and max 5000) |
| `GET` | `/photos/{id}/faces` | the photo's faces, no embeddings |
| `GET` | `/faces/{id}/image` | the face cut from its photo's thumbnail with a third of its size around it, a JPEG; a purged photo's is a **410** |
| `GET` | `/photo-person-statuses` | `["suggested","confirmed","rejected"]` |
| `GET` | `/photo-people` | who is in which live photo: `?photo_id=`, `?person_id=`, `?status=`, `?limit=`, `?offset=` |
| `POST` | `/photo-people` | `{photo_id, person_id, face_id, status, confidence, evidence, proposed_by, decided_by}`: `person_id` is people-store's (`person_000001`); `status` is `suggested` or `confirmed`, which needs `decided_by`; `face_id` must be on the photo. For a pair already linked it only moves a suggestion upward: a confirmed or **rejected** link is answered as it stands (200), so no matcher undoes a person's rejection. **201** when created |
| `PATCH` | `/photo-people/{id}` | any of `status`, `decided_by`, `face_id`, `evidence`; a status change needs `decided_by`. Every change keeps the old row in `photo_person_revisions` |
| `GET` | `/people-counts` | `[{person_id, confirmed, suggested}]` over live photos, rejected links left out, most confirmed first |
| `GET` | `/face-group-statuses` | `["open","named","not_known","mixed"]` |
| `GET` | `/face-groups` | groups of faces a grouper found to be one person, the most photos first: `?status=`, `?person_id=`, `?limit=` (default 100), `?offset=`. Each has `face_count`, `photo_count`, `first_taken_at`, `last_taken_at`, up to 12 `sample_face_ids` (one per photo, spread evenly from the most typical to the least, so an odd face shows) and its `guesses`, newest first |
| `POST` | `/face-groups` | `{model, made_by, members:[{face_id, similarity}]}`: a new open group. A face that is retired, on a deleted photo or found by another model is a 400; a face already in a group is a **409**. **201** with the group |
| `GET` | `/face-groups/{id}` | one group with every face in `members` (`face_id`, `photo_id`, `similarity` to its average face, `taken_at`, `folder`) |
| `POST` | `/face-groups/{id}/members` | `{members:[{face_id, similarity}]}`: faces that belong to an **open** group; a decided group is a 409 |
| `POST` | `/face-groups/{id}/guesses` | `{person_id, confidence, evidence, proposed_by}`: who a model or the context thinks it is; `person_id` `""` means nobody in people-store. Kept as written. **201** |
| `POST` | `/face-groups/{id}/decision` | `{status, person_id, decided_by, excluded_face_ids}`: a person saying who it is. `excluded_face_ids` leave the group first, free to be grouped again. `named` (needs `person_id`) confirms every remaining face as the person through the rules of `POST /photo-people` (a rejection holds, a confirmed link stays) and rejects suggestions of anyone else on those faces; `not_known` is someone not to name; `mixed` is more than one person. An open group can be decided, a `not_known` or `mixed` one can still be named, and a `named` one can only be named as someone else with nothing left out, which rejects the first person's links on its faces (`previous_person_rejected`); anything else on a named group is a **409**. Answers `{group, confirmed, already_confirmed, kept_rejected, others_rejected, excluded, previous_person_rejected}` |
| `GET` | `/photos` | live photos, oldest taken first. `?from=` (inclusive) `&to=` (exclusive) on `taken_at`, `?folder=`, `?q=` (every word in the description or tags), `?tag=`, `?person=` (a `person_…` confirmed in it), `?described=true|false`, `?has_gps=true`, `?sha256=` (the original file's hash: the one photo with it, or none), `?include_deleted=true`, `?exclude_tag=` (repeatable: leave out photos carrying it), `?limit=` (default 100, max 1000), `?offset=` |
| `GET` | `/taken-activity` | live photos counted per span of `taken_at`, earliest first: `[{start, photos, sample_photo_ids}]`. `?interval=` `day`, `week` (from Monday), `month` or `year`; `?time_zone=` an IANA zone, required, in which spans start (`start` is epoch seconds); `?samples=` 0–24 ids per span, spread evenly through it; the same `from`, `to`, `folder`, `q`, `tag`, `person`, `described`, `has_gps` and `exclude_tag` as `/photos`. Photos with no `taken_at` are not counted |
| `POST` | `/imports` | `{"photos":[ImportPhoto…]}` with `thumbnail_jpeg` as base64. Upsert by `sha256`: new is created, known with the same facts is skipped, known with other facts is updated after a revision. **Description and tags are never touched by an import.** One bad photo is a 400 naming its index and writes nothing. A known photo whose thumbnail arrives with other bytes has its faces retired and its scans forgotten, so it is scanned again; `faces_retired` in the answer counts them |
| `GET` | `/photos/{id}` | deleted is 404 unless `?include_deleted=true` |
| `PATCH` | `/photos/{id}` | any of `description`, `described_by`, `tags`, `taken_at`, `date_source`; any other key or `null` is a 400 |
| `DELETE` | `/photos/{id}` | soft, **204** |
| `POST` | `/photos/{id}/restore` | a purged photo is a **409** |
| `POST` | `/photos/{id}/purge` | removes the photo for good, for a picture that must not be kept (an ID, a card, a document): deletes its thumbnail file, blanks its description in the row and every revision, marks it deleted and purged, and makes every later import skip it. The row stays so a citation still resolves. Again is a no-op. Answers the photo |
| `GET` | `/photos/{id}/revisions` | newest first, each the whole row as it was |
| `GET` | `/photos/{id}/thumbnail` | the JPEG; served for a deleted photo too, so a citation keeps drawing; a purged photo is a **410** |

## Importing

`~/bin/photo-import -dir <dir>` posts a directory prepared on the machine that
holds the originals: `index.jsonl`, one line per photo with the fields of
`ImportPhoto` and `thumbnail` naming its JPEG inside the directory, and the
thumbnails. Batches of 50; running it again changes nothing that did not change.

`~/bin/photo-import-messages [-max-members 8] [-discord-archive <dir>] [-dry-run]`
imports the photos people sent in the user's chats, read through multichat
(`MULTICHAT_URL`, `MULTICHAT_TOKEN`): every image in the message log of a room
with at most `-max-members` members, never a Discord server channel, plus with
`-discord-archive` the image attachments `multichat-fetch-discord-history`
saved. Each photo's `source_machine` is `multichat`, its `source_path` the
`message_…` ref it came in (a chip), its `folder` `messages/<platform>/<room>`
or `discord/<server>/<channel>`, and its date when it was sent
(`date_source` `message`). Thumbnails are made in Go (package `thumbnail`, public so another repo's importer makes the same ones:
JPEG, PNG, GIF, WebP; longest side 1024, quality 80).
