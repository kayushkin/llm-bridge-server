# activity-store contract

activity-store runs inside llm-bridge-server, at `/online-activity/` with the
paths below, and keeps its records in Postgres (schema `activity`, role
`activity_module`) and the bytes of its attachments in the server's file-store
(owner service `activity-store`, owner ref the activity id). It answers only a
request carrying the caller the server verified; one with none is 401. What a
person did online, one activity per thing done at one time, imported from
Google Takeout. The wire types are in `activity.go` and `vocabulary.go`,
rendered to `ts/model.ts`.

## Owners

Every record has an owner, `owner_principal_id` (principal-store's
`principal_000001`), the person whose activity it is.

- `POST /imports` starts an import owned by the caller, or answers the
  caller's own import of a file with that sha256: the hash is unique per
  owner. A caller that acts as no principal cannot start one (403).
- Activities, sightings, attachments and revisions take their import's owner.
  An import matches records only among its owner's own: a sighting's
  `(source, source_ref)` and an activity's dedupe key are unique per owner, so
  two people's identical records are two activities and never fold together.
- A caller reaches only its own records. A record of another owner answers
  404, as a missing one does, on reads and writes alike, an attachment
  included; lists, `/stats`, `/days/{date}`, `/imports` and `/health` hold
  only the caller's own. An administrator, or the server's own service,
  reaches every owner's, and `?owner_principal_id=` narrows any read to one
  owner. Anyone else may name only itself there (403 otherwise).

## Records

### `activities` (`activity_000001`)

| Field | |
|---|---|
| `product` | Google's name for where it happened, as the export names it: `Search`, `YouTube`, `Maps`, `Chrome`, `Google Play Store`… An open set |
| `action` | one of `GET /vocabularies` `actions`: `searched`, `visited`, `watched`, `viewed`, `used`, `directions`, … `other` |
| `action_text` | the export's own words for the verb (`Searched for`, `Directions to`); empty when the card had none |
| `title` | the query, the page or video title, the app, or the whole card's words when no verb was found |
| `url` | the link the card or record gave; may be empty |
| `occurred_at` | epoch seconds, UTC |
| `latitude`, `longitude` | where the user was, from the first My Activity `Locations` line that gives coordinates; `null` otherwise. Not where a Maps search pointed |
| `details` | `{header, products, notes, locations:[{name, latitude, longitude, basis, url}], channel:{name,url}, lines:[{text,url}], chrome_client_id, page_transition, bookmark_folder}`, each field only when present |
| `source`, `source_ref` | the source that first brought it in, and its reference there |
| `import_id` | that first import |
| `sightings` | every source record that described it, the first included: `[{source, source_ref, product, action, import_id, occurred_at, seen_at}]` |
| `attachments` | `[{id, activity_id, file_name, content_type, sha256, size_bytes, created_at, purged_at}]`; the bytes are file-store's |
| `deleted_at`, `purged_at` | epoch seconds; 0 = not. A purged activity is also deleted |

`source_ref` never holds the record's words: for My Activity it is a hash of
the product, the card's first line and links, and its time (with `#2`, `#3`
for repeats within one page); for Chrome history `<time_usec>-<url hash>`; for
bookmarks `<add_date>-<url hash>`.

### `imports` (`import_000001`)

`{id, owner_principal_id, kind, sha256, source_machine, source_path, imported_at, finished_at, counts:{created, folded, known, attachments_stored}}`.
One file is one import, found again among its owner's imports by its sha256.

## Routes

| Method | Path | |
|---|---|---|
| `GET` | `/health` | `{status, counts:{activities, deleted, purged, sightings, attachments, imports, revisions}}`, counted in the caller's reach |
| `GET` | `/vocabularies` | `{actions, action_phrases:[{phrase, action}], sources, import_kinds, revision_reasons}` |
| `GET` | `/stats` | live activities: `{total, first_at, last_at, by_product, by_source, by_action, sightings_by_source}`, each `[{name, count}]` most first |
| `GET` | `/activities` | `?from=&to=` (epoch seconds on `occurred_at`, from inclusive, to exclusive), `?product=`, `?action=`, `?source=` (any sighting from it), `?q=` (full text: every word must appear whole in the title, url or details, in any case; punctuation separates words; unlike the standalone store's FTS5, an accented letter matches only itself), `?include_deleted=true`, `?order=asc\|desc` (default oldest first), `?limit=` (default 100, max 1000), `?offset=`. Answers `{activities, total, limit, offset}`. An unknown parameter, action or source is a **400** |
| `GET` | `/activities/{id}` | a deleted activity is **404** unless `?include_deleted=true` |
| `DELETE` | `/activities/{id}` | soft, after a revision; **204** |
| `POST` | `/activities/{id}/restore` | after a revision; a purged activity is a **409** |
| `POST` | `/activities/{id}/purge` | for something that must not be kept: blanks the title, url, words, details and location in the row and in every revision, empties its search document and destroys its attachment files in file-store, marks it deleted and purged. The row, its sightings and its dedupe key stay, so a citation still resolves and no later import brings it back. Again is a no-op. Answers the activity |
| `GET` | `/activities/{id}/revisions` | newest first, `[{id, activity_id, reason, revised_at, activity}]` |
| `GET` | `/attachments/{id}` | the file with its content type; a purged one is **410** |
| `GET` | `/days/{YYYY-MM-DD}` | `?tz=` (required, an IANA zone) `&first=` (default 5, max 50): the day from midnight to midnight in that zone, `{date, timezone, from, to, total, products:[{product, count, first_at, last_at, actions:[{name,count}], first:[{id, occurred_at, action, title, url}]}]}`, products with the most activities first |
| `GET` | `/imports` | newest first |
| `POST` | `/imports` | `{kind, sha256, source_machine, source_path}`: starts an import, or answers the one with that hash: `{import, already_finished}` |
| `GET` | `/imports/{id}` | |
| `POST` | `/imports/{id}/batches` | `{activities:[ImportActivity]}`, one transaction: one bad record is a **400** naming it and writes nothing. A finished import is a **409**. Answers `{created, folded, known, attachments_stored}` |
| `POST` | `/imports/{id}/finish` | stamps `finished_at`; the same file is then a no-op |

## How a record is matched

Matching is within the import owner's records only.

1. A record whose `(source, source_ref)` is already a sighting is **known**
   and changes nothing.
2. Otherwise, a record with a url is **folded** into an activity with the same
   url and the same second (`dedupe_key`, the first 128 bits of sha256 of
   both) when that activity came from another source, or from the same
   product in another import (a later export), and none of the activity's
   sightings came from this import. It becomes a sighting of that activity;
   attachments it carries are added unless the activity was purged.
3. Anything else is a new activity.

So a YouTube watch in My Activity and in YouTube's own history is one
activity with two sightings, and a page in My Activity's Chrome page and in
Chrome's `History.json` is one. Two products of one source (a result clicked
in Search, and Chrome's record of the page) stay two activities, as Google
keeps them.

## Importing

`activity-import-google -dir <takeout dir> [-source-machine <name>] [-dry-run]`
reads, each file as its own import:

- every `My Activity/<Product>/*.html`, filed under the directory's name, with
  the recordings and pictures each card names (`<audio><source src>`,
  `<img src>`) sent as attachments;
- `YouTube and YouTube Music/history/*.html` (watch and search history, the
  same HTML), filed under `YouTube`, source `youtube_history`;
- `Chrome/History.json` (`Browser History` only), source `chrome_history`;
- `Chrome/Bookmarks.html`, each dated bookmark as `bookmarked`, source
  `chrome_bookmarks`.

It posts to `ACTIVITY_STORE_URL` (default `http://127.0.0.1:8336`, where the
standalone store answered; point it at the server's `/online-activity/`) in
batches of 2000 and prints, per file, what the store did, then names any
file beside the My Activity pages that no card names.

### Times

My Activity writes each time as `Oct 26, 2026, 7:01:10 PM PDT`. Takeout
writes every time in a page with the abbreviation of the zone the account had
when the export was made, and applies that abbreviation's offset to every
date, summer or winter. The reader therefore takes the abbreviation as a
fixed offset (`internal/takeout/googletime.go`), never as a zone with
daylight saving, and refuses one it does not know or that two zones share.

## Moving a standalone store in

`llm-bridge-server import-activity-store` (the server's command) runs
`ImportStandaloneDatabase`: it copies a standalone SQLite file (a `.backup`
copy) and its `attachments` folder into an empty module for one owner,
keeping every id and numbering on after the highest. Each live attachment's
file is checked against its row's sha256 and size and handed to file-store; a
purged one has none. The FTS5 table is not copied: each search document is
made again from its activity, and a purged activity's is empty.
