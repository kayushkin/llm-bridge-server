# message-store routes

message-store runs inside llm-bridge-server, at `/message-store/` with the paths below, and keeps its records in Postgres (schema `messages`, role `messages_module`). It answers only a request carrying the caller the server verified; one with none is 401. All JSON. Times are unix milliseconds, UTC. Errors are `{"error": "…"}`: 400 for a bad request (an unknown field included), 404 for a missing id.

## Owners

Every record has an owner, `owner_principal_id` (principal-store's `principal_000001`), the person whose messages these are, and every record in the answers says so.

- `POST /imports` starts a run owned by the caller, and `POST /accounts` puts an account of the caller's. A caller that acts as no principal (the server's own service on its own) cannot make either (403).
- Everything a run's batches write — conversations, participants, messages, days, superseded rows, revisions, merges — is the run's owner's, and every key a batch names (a conversation, a message to supersede, a merge's two sides) is looked up in that owner's records only. `(source, source_ref)` is unique per owner, so two people may each hold the same Discord message; an account's `(platform, handle)` likewise.
- A caller reaches only its own records. A record of another owner answers 404, as a missing one does, on reads and writes alike, retired ids included (no 301 to another owner's row); lists, `/summary`, `/activity` and `/conversation-days/journal-entries` hold only the caller's own. An administrator, or the server's own service, reaches every owner's, and `?owner_principal_id=` narrows any read to one owner. Anyone else may name only itself there (403 otherwise).
- `?account=` on `GET /conversations` reads an account in reach, and lists only that account's owner's conversations.

## Ids

| Record | Id |
|---|---|
| conversation | `conversation_000001` |
| message | `chat_message_000001` |
| conversation day | `conversation_day_000001` |
| import run | `message_import_000001` |
| account | `message_account_000001` |
| person | people-store's `person_000001`, never made here |

A retired id (a superseded message, a merged conversation) is never handed out again.

## Search

`q` on `GET /messages` reads words as the standalone store's FTS5 index did: each whitespace-separated word must appear whole (no prefix match), case blind, with every ASCII punctuation mark splitting words, so `well-being` is the phrase `well being`; a query of punctuation alone matches nothing. One difference: FTS5 dropped accents (`café` matched `cafe`), and Postgres's `simple` configuration does not.

## Routes

| Route | What it does |
|---|---|
| `GET /health` | `{"status":"ok"}` when the database answers |
| `GET /vocabulary` | platforms, message kinds, activity groupings and intervals |
| `GET /summary` | conversations and messages per platform, with first and last times |
| `GET /messages` | messages in time order. `conversation` (repeatable; a merged id reads the conversation it merged into), `person`, `platform`, `source` and `source_ref` (the one message an importer filed under that ref, such as a Discord message id; `source_ref` alone is a 400), `after`, `before`, `q` (full text), `order=oldest\|newest`, `limit` (≤5000, default 500), `cursor` (the last page's `next`) |
| `GET /messages/{id}/revisions` | every earlier version of a message, oldest first: `import_id`, `revised_at` and the whole `row` as it was before that import changed it |
| `GET /messages/{id}` | one message; a superseded id is a 301 whose relative `Location` is the message that holds it now |
| `GET /superseded-messages` | retired rows, newest first, each with its whole old row as `row`. `superseded_by`, `limit` |
| `GET /accounts` | accounts of ours, the user's own first: `id` (`message_account_000001`), `platform`, `handle` (the user id there), `display_name`, `kind` (`GET /vocabulary` `account_kinds`: `user`, `bot`) |
| `GET /accounts/{id}` | one account |
| `POST /accounts` | `{"platform","handle","display_name","kind"}`: the account with that platform and handle, made if new; an empty display name keeps the stored one |
| `GET /conversations` | most recently active first. `account` (the conversations with a participant of the account's handle on its platform), `person`, `platform`, `q` (title or participant name), `limit` |
| `GET /conversations/{id}` | one conversation, participants and message summary; a merged id is a 301 whose relative `Location` is the conversation it merged into |
| `GET /conversation-merges` | merged conversations, newest first, each with its whole old row and participants as `row`. `merged_into`, `limit` |
| `PATCH /conversations/{id}/participants/{handle}` | `{"person_id":"person_000001"}` names the handle's person; `""` clears it |
| `GET /conversation-days` | `conversation`, `journal_entry`, `person`, `from`, `to` (YYYY-MM-DD), `limit`; `involved=true` keeps only days the user took part in. Each day says its `involvement` (`GET /vocabulary` `day_involvements`): `direct` (not a group), `participated` (the user wrote that day), `addressed` (someone replied to them or mentioned them, Discord `<@id>`), or `none` |
| `GET /conversation-days/{id}` | the day, its conversation and its messages |
| `POST /conversation-days/journal-entries` | `[{"journal_source","journal_source_ref","journal_entry_id"}]`: records which journal entry held each day's transcript; answers `linked` and `unmatched` |
| `GET /activity` | message counts per time bucket. `group_by=person\|conversation\|platform`, `interval=day\|week\|month\|year`, `time_zone` (IANA, required), `person`, `after`, `before`; `involved=true` leaves out a group's messages on local days (in `time_zone`) the user neither wrote in it nor was replied to or mentioned, judged from the messages, so chats with no conversation days count the same way, so a channel the user only belonged to does not fill a timeline |
| `GET /imports`, `GET /imports/{id}` | import runs and what each wrote |
| `POST /imports` | `{"importer","description"}` starts a run |
| `POST /imports/{id}/batches` | `{"conversations","messages","conversation_days","supersessions","conversation_merges"}`, written in one transaction: one bad record writes nothing |
| `POST /imports/{id}/finish` | ends a run |

## Import rules

- A conversation is found by `(source, source_ref)`. An empty title in a batch
  keeps the stored one. Participants are added or updated by handle; a
  participant's `person_id` is written only when the batch gives one, so a
  person set by hand survives an import that knows nobody.
- A message's `body_source` is required: `platform` (the app's own text) or
  `bridge` (a bridge's rendering). A bridge's text never replaces stored
  platform text; any other non-empty text replaces the stored text.
- A message's `embeds` and `buttons` (a bot's cards and the buttons under a
  message) are replaced by an import that sends any and kept by one that sends
  none.
- A participant's `is_self` is set by an import that sends it and never
  cleared by one that does not.
- A message is found by `(source, source_ref)`. The same `source_hash` skips it;
  another hash rewrites it. A sender handle the conversation never listed gets a
  participant row.
- A message's `supersedes` lists `(source, source_ref)` keys of rows that hold
  the same message under another id namespace. Each that exists is copied
  whole to `superseded_messages` (with the import and the message that
  replaced it) and removed from `messages`; its id then redirects. A batch
  counts them as `messages_superseded`. A message sent again under a
  superseded key is skipped and counted as `messages_superseded_skipped`.
- A batch's `supersessions` (`[{"superseded":{source,source_ref},"by":{source,source_ref}}]`,
  applied after its messages) retire a row in favor of a message already
  stored, for an importer that does not own that message. A `by` that is not
  stored is a 400; a `superseded` that is not stored, or already retired, is
  nothing to do.
- A batch's `conversation_merges` (`[{"merged":{source,source_ref},"into":{source,source_ref},"rule"}]`,
  applied after its supersessions and before its days) make two conversations
  one. `rule` is one of `GET /vocabulary`'s `conversation_merge_rules`. The
  merged conversation's messages, days and participants move to the other (a
  handle in both keeps what the survivor knows and learns what it does not),
  its row and participants are copied whole to `conversation_merges`, and it is
  removed; its id then redirects. Its `(source, source_ref)` resolves to the
  survivor on every later import, which adds participants but never changes
  the survivor's title, platform or `is_group`. Either key may name a
  conversation merged earlier. A side the store does not hold is skipped and
  counted (`conversation_merges_skipped`); two days on the same date in both
  conversations is a 400.
- A conversation day is found by `(conversation, day)`; its span is computed from
  `day` and `time_zone`.

## Activity by person

A message counts for a person when that person sent it (`received`), or when
the user sent it in a direct conversation with that person (`sent`). The user's
messages in a group are credited to nobody.
