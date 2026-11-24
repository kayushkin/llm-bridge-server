# people-store — API contract

`127.0.0.1:8321`. The registry of the people in the operator's own life —
friends, family, coworkers — for a journal and for messages. This store hands
out the id everything else joins on: `person_000001`.

principal-store (`:8314`) stays the registry of who may log in, hold a grant or
be assigned a card. A person may point at a principal with `principal_id`; this
store never copies a principal's name or email.

Module `github.com/kayushkin/people-store`, root package `peoplestore`, server
in `cmd/people-store`, importers in `cmd/people-import-multichat` and
`cmd/people-import-contacts`, all installed to `~/bin`. SQLite at `<data dir>/people-store.db`, WAL, foreign keys on. Build
and test with `-tags sqlite_fts5`.

**No auth, so the bind is `127.0.0.1` and that is part of this contract.**
`deploy.sh` fails if the port listens anywhere else. All bodies are JSON; all
timestamps are epoch seconds (0 = unset). Errors are `{"error": "…"}`.

| Status | Means |
|---|---|
| 400 | the body or a parameter is wrong, including an unknown JSON key on any write |
| 404 | no such record |
| 409 | the write contradicts a row: a handle owned by someone else, a deleted person, a decided suggestion, a relationship already recorded |
| 502 | principal-store could not say whether a `principal_id` exists, so the write was refused |

## Ids

`person_000001`, `identity_000001`, `relationship_000001`,
`suggestion_000001`, `revision_000001`, `refusal_000001`: a prefix and a
zero-padded sequence.
The prefix lets kanban-store's entity-type registry claim `person_\d{6,}`
without colliding with noteboard's bare uuids.

## Records

### Person

| Field | Meaning |
|---|---|
| `id` | `person_000001` |
| `display_name` | required, trimmed. Display only; never a join key, not unique |
| `birthday` | `""`, `"YYYY-MM-DD"`, or `"--MM-DD"` when the year is unknown (the vCard 4 form). A date that does not exist is a 400; `--02-29` is allowed |
| `how_met` | free text |
| `tags` | array of strings, trimmed and de-duplicated; `[]` when none |
| `principal_id` | `""` or a principal-store id. Must match `principal_\d+` **and** answer 200 at principal-store's `GET /principals/{id}`; a 404 there is our 400, anything else our 502 |
| `created_at`, `updated_at`, `deleted_at` | `deleted_at` 0 = live |

`GET /persons/{id}` and every write to a person answer a **PersonDetail**: the
fields above plus `identities` and `relationships`, each `[]` when empty.
`GET /persons` leaves both out.

### Identity

A handle that reaches a person. `{id, person_id, kind, value, label, linked_by, reason, linked_at, created_at, updated_at}`.
**One handle belongs to at most one person**: `UNIQUE(kind, value)` on the
normalized value. **Every link says who made it and why**: `linked_by` is one
of `GET /link-sources`, `reason` is the linker's words, `linked_at` when the
link was made (an import may give an earlier time than `created_at`). Handles
attached before 2026-10-01 carry `linked_by: import` and a reason that says so.
Kinds are served by `GET /identity-kinds`:

| Kind | Normalized to |
|---|---|
| `email` | trimmed, lowercased; must hold an `@` |
| `phone` | `+` and digits. Input must start with `+` (international form); spaces, `-`, `.`, `(`, `)` are dropped; 7–15 digits |
| `matrix_user` | trimmed; must look like `@localpart:server` — multichat's id for one bridged chat account, e.g. `@telegram_123456789:matrix.example.com` |
| `discord` | trimmed |
| `url` | trimmed |

### Link sources

`GET /link-sources` → `[{source, overrides_refusals, reason_required, description}]`.

| Source | Who | Overrides refusals | Reason |
|---|---|---|---|
| `operator` | the operator, in dash `/people` or multichat's person picker | yes | optional |
| `phone_number` | multichat's phone-number matcher | no | required |
| `agent` | the contact review agent | no | required |
| `import` | a one-off copy from another store; every handle from before provenance | no | required |

A source that does not override refusals gets a **409** for a refused pair and
may not move a handle (**400**). Accepting or rejecting a suggestion and
removing a handle are the operator's acts.

### Refusal

"This handle is not this person": `{id, kind, value, person_id, refused_by, reason, created_at}`,
`UNIQUE(kind, value, person_id)`. Written when the operator removes a handle
from a person (`DELETE /identities/{id}`), moves it away from them, or rejects
a suggestion whose `matched_person_ids` name them (one row per handle and
matched person). The operator linking the pair again deletes it. A refusal is
about one person only: the handle may still go to anyone else.

### Relationship

Stored once per fact as `{id, from_person_id, to_person_id, kind, note, created_at}`,
read "from_person is to_person's kind". Kinds and inverses from `GET /relationship-kinds`:
partner↔partner, spouse↔spouse, parent↔child, sibling↔sibling, friend↔friend,
coworker↔coworker, other↔other.

On a person, each relationship is shown from that person's side as
`{id, person_id, person_display_name, person_deleted_at, kind, note, created_at}`:
`person_id` is the other person and `kind` is **what they are to this one**. A
row (Mother, Son, parent) shows on Son as `{person_id: Mother, kind: parent}`
and on Mother as `{person_id: Son, kind: child}`.

### Suggestion

A source's guess that some handles are one person.
`{id, source, payload, status, person_id, created_at, decided_at, activity}`;
`payload` is `{display_name, identities, matched_person_ids, source_detail, reason?, covered?}`.
`activity` is `{last_activity_at, message_count, direct_chat}`, worked out on
each read from the handle activity of the suggestion's handles: the newest
`last_activity_at`, the sum of `message_count`, and whether any has
`direct_chat`; all zero when no source reported any of them.
`status` is `proposed`, `accepted`, `rejected` or `superseded`. **This is the
one queue of guesses about who is who.** Nothing in a suggestion is attached
until the operator accepts it; a direct link (`POST /persons/{id}/identities`
from any source) is the other way in.

Sources are `multichat`, `google_contacts` and `phone_messages`, **importers**
(`~/bin/people-import-multichat`, `~/bin/people-import-contacts`, and
journal-store's `journal-import-android-messages` for numbers in a phone's
text messages), and
`agent`, a **reviewer** (the contact review agent):

- An importer never proposes a handle twice: one held by any suggestion,
  whatever its status, is skipped. So a second run proposes only what is new
  and a rejected guess is never proposed again.
- A reviewer must give a `reason`; it skips only handles in its own suggestions
  that still wait, and gets a **409** when a handle it proposes is refused for
  one of the `matched_person_ids`. To say "this handle is that person", it
  proposes both that handle and one the person already has: the owned one is
  skipped and names the person in `matched_person_ids`.

**When a handle in a waiting suggestion is attached to a person some other
way**, it moves from `payload.identities` to `payload.covered`
(`[{kind, value, person_id, covered_at}]`) and that person joins
`matched_person_ids`. A suggestion left with no handle becomes `superseded`,
`decided_at` set, with `person_id` set when every covered handle went to one
person.

### Handle activity

What a source saw of one handle, keyed like an identity on the normalized
`(kind, value)`: `{kind, value, last_activity_at, message_count, direct_chat}`,
stored with the `source` that reported it and `observed_at`. A fact about the
handle, not about a suggestion or a person; suggestions read it. `last_activity_at`
is epoch seconds (0 = none seen), `message_count` the messages the handle sent
as the source logged them, `direct_chat` whether the operator has a one-to-one
chat with it. Each report replaces a handle's row whole. Sources are those of
suggestions (`multichat`). No route reads it directly.

### Review queue

The contact review agent's work list: every handle with handle activity that
shows the operator talks with it (`message_count > 0` or `direct_chat`), that
nobody has reviewed and nobody claimed in the last 30 minutes, one-to-one chats
first, then newest activity, then most messages. Each entry is
`{kind, value, person_id, last_activity_at, message_count, direct_chat}`,
`person_id` `""` when the handle is attached to no one. A claim keeps a handle
out of the queue for 30 minutes so two sessions never take the same one; a
review keeps it out for good. Claimants and reviewers are link sources. The
queue is only as fresh as the last handle-activity report.

### Revision

`{id, person_id, action, snapshot, created_at}`. `snapshot` is the person with
their identities **as they were just before** `action` changed them. Actions:
`update`, `delete`, `restore`, `identity_add`, `identity_remove`.

## Routes

| Method | Path | Body / query | Answer |
|---|---|---|---|
| GET | `/health` | | `{"status":"ok","counts":{persons, deleted_persons, identities, relationships, proposed_suggestions, refusals}}` |
| GET | `/settings` | | llm-bridge `servicesettings` description; nothing is editable |
| GET | `/identity-kinds` | | `[{kind, description}]` |
| GET | `/relationship-kinds` | | `[{kind, inverse, description}]` |
| GET | `/link-sources` | | `[{source, overrides_refusals, reason_required, description}]` |
| GET | `/persons` | `q` (full-text, prefix per word, over name, how_met, tags and identity values and labels), `tag`, `principal_id` (the person pointing at that principal; not of the form `principal_000001` is 400), `include_deleted=true`, `limit` (default 100, max 1000), `offset` | `[Person]` by name |
| POST | `/persons` | `{display_name, birthday?, how_met?, tags?, principal_id?, identities?: [{kind, value, label?, linked_by, reason?, linked_at?}]}` | 201 PersonDetail. The identities attach in the same transaction: a handle owned by someone else, or one named twice, refuses the whole write |
| GET | `/persons/{id}` | | PersonDetail. **A deleted person answers 200 with `deleted_at` set**, so whatever points at them still resolves; unknown is 404 |
| PATCH | `/persons/{id}` | any of `display_name`, `birthday`, `how_met`, `tags`, `principal_id`; `null` clears all but `display_name` | PersonDetail. Unknown key 400 (naming the accepted ones); `id`, `deleted_at`, `identities`, `relationships` refused naming the route that changes them; a deleted person 409 |
| DELETE | `/persons/{id}` | | PersonDetail with `deleted_at` set. Soft: identities stay attached. Already deleted 409 |
| POST | `/persons/{id}/restore` | | PersonDetail. Not deleted 409 |
| GET | `/persons/{id}/revisions` | | `[Revision]` oldest first |
| POST | `/persons/{id}/identities` | `{kind, value, label?, linked_by, reason?, linked_at?, move?}` | 201 Identity; 200 when this person already had it (its provenance unchanged); 409 naming the owner when someone else has it, unless `move: true` from the operator, which takes it and refuses it for the person it left; 409 for a refused pair from a source that does not override refusals; 409 on a deleted person. No `linked_by`, a missing required `reason`, a future `linked_at`, or `move` from another source is 400 |
| GET | `/identities` | `kind`, `limit` (default and max 1000), `offset` | `[Identity]` in the order attached, for a caller that maps every handle of a kind to its person |
| DELETE | `/identities/{id}` | | 204. The operator's act: the row goes, the revision keeps it, and a refusal of the handle for that person is written |
| GET | `/refusals` | `person_id`, `kind`, `value` (needs `kind`; normalized) | `[Refusal]` oldest first |
| GET | `/identities/resolve` | `kind`, `value` (normalized as on write) | `{identity, person}`; 404 when nobody has it |
| POST | `/persons/{id}/relationships` | `{person_id, kind, note?}` — "person_id is this person's kind" | 201 Relationship. Self 400; either side deleted 409; the same fact from either side again 409 |
| DELETE | `/relationships/{id}` | | 204 |
| GET | `/suggestions` | `status`, `q` (substring of `display_name`, ASCII case ignored), `sort` (`created`, the default: oldest first; `activity`: people the operator talks to first — any handle with `direct_chat` first, then newest `last_activity_at`, then most messages, then oldest; anything else 400 naming both), `limit` (default 100, max 1000), `offset` | `[Suggestion]` |
| POST | `/suggestions` | `{source, display_name, identities:[{kind,value,label}], source_detail?, reason?}` (reason required from `agent`) | 201 `{created:true, suggestion, skipped}`; 200 `{created:false, suggestion:null, skipped}` when every handle is attached or already suggested |
| GET | `/suggestions/{id}` | | Suggestion |
| POST | `/suggestions/{id}/accept` | optional `{person_id}` | `{suggestion, person}`. With no `person_id` a new person is made, named after the suggestion. Each handle is linked by `operator` with reason `accepted suggestion_…` (and the suggestion's reason). All or nothing. Not `proposed` 409 |
| POST | `/suggestions/{id}/reject` | | Suggestion. Refuses each of its handles for each matched person. Not `proposed` 409 |
| GET | `/reviews/pending` | `limit` (default 25, 1–1000) | `{total, active_claims, pending: [PendingReview]}` |
| POST | `/reviews/claim` | `{limit (1–100), claimed_by}` | `{claimed: [PendingReview], remaining}`; the claimed handles leave the queue for 30 minutes |
| POST | `/reviews` | `{handles: [{kind, value}], reviewed_by, reviewed_at?}` | `{reviewed}`; ends their claims |
| PUT | `/handle-activity` | `{source, activities:[{kind, value, last_activity_at, message_count, direct_chat}]}` | `{updated}`. Each handle is normalized and its row replaced, all in one transaction. Unknown source, a bad handle, a negative number, a handle named twice or an unknown key 400, and nothing is written |

## Chat reference chips

kanban-store's entity-type registry carries `{type: person, service:
people-store, search: /persons?q=, get: /persons/{id}, id_patterns:
[person_\d{6,}]}`. dash's `POST /api/resolve` probes `get` for every id that
matches and passes a 200 body through as the chip's data; a 404 is an honest
miss. Nothing else is needed from this store. dash also needs `people-store` in
its `resolveServiceURLSetting` map before it can probe.

## The multichat importer

`~/bin/people-import-multichat` reads multichat's `GET /api/contacts/unified`
(token from auth-store provider `multichat`) and posts one suggestion per
group: each member account becomes a `matrix_user` handle labeled with its
platform. Since 2026-10-01 multichat groups by this store's `person_id`, so a
linked group's handles are already attached and are skipped; what is left is
mostly single unlinked accounts. Run it on a schedule (hourly, say); that is
also what keeps the review queue's activity fresh.

Then it reports activity (`PUT /handle-activity`, 500 handles a call) for
every handle in the contact list. From `GET /api/message-log`, paged whole
with `limit=1000` and `before` (edits left out, as multichat does by default),
it counts each sender's messages and keeps the newest `sent_at`. From
`GET /api/conversations`, a room with `direct_message_partner_user_id` marks
that handle `direct_chat` and lends it the room's `last_activity` (milliseconds)
when that is newer. Only handles in the contact list are reported: multichat
leaves our own puppets out of that list, and the log does not mark them.

Environment, all required: `PEOPLE_STORE_URL`, `MULTICHAT_URL`,
`AUTH_STORE_URL`, `AUTH_STORE_TOKEN`. It prints one line per suggestion made
and a JSON summary with the activity counts, and exits 1 on any failure; when
a group fails, it reports no activity.

## The Google Contacts importer

`~/bin/people-import-contacts -export-dir DIR [-north-american-numbers]
[-include-other-contacts] [-dry-run] [-sample N]` reads every `.vcf` under a
Google Takeout Contacts directory and posts one suggestion, source
`google_contacts`, per contact (cards merged when they share their name words
and a phone or email) that has a phone or email no person has and no
suggestion holds. It sends all the contact's handles; the store leaves out
owned ones and names their owners in `matched_person_ids`. `reason` says the
card's name, whether it is starred, any person whose name matches when no
handle does (a hint only), the organization and a birthday to set by hand.
`source_detail` is `{formatted_name, structured_name, nicknames,
organization, birthday, birthday_raw, note, categories, starred, has_photo,
phones: [{raw, e164, label, skipped}], emails: [{raw, address, label}], files,
name_matches: [{person_id, display_name}]}`. Before writing it reads
`GET /persons`, `GET /identities?kind=phone|email` and every suggestion, so
`-dry-run` prints what a run would do: counts by outcome (`suggest`,
`already_known`, `already_proposed`, `skipped_no_handle`,
`skipped_other_contact`, `skipped_no_name`) and by suggestion kind
(`extends_person`, `name_match`, `new_person`), with examples. Environment:
`PEOPLE_STORE_URL`. Exits 1 when any proposal fails.
