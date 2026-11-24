# principal-store — API contract

The registry of principals: the humans and groups a permission can be granted
to and a card can be assigned to. This store hands out the id everything else
joins on.

Package `internal/access/principalstore` of llm-bridge-server. The bridge
mounts every route below under `/principal-store/` (so `GET /principals/{id}`
is `GET /principal-store/principals/{id}`), decides who may call each one, and
keeps the records in the Postgres module `principals` (role
`principals_module`). Until the landing day the standalone service answers the
same routes at the root of `127.0.0.1:8314` from its SQLite file, with no auth
of its own. All timestamps are epoch seconds (0 = unset).

---

## The tables

### `principals`

| Field | Meaning |
|---|---|
| `id` | `principal_000001`. What every other store joins on |
| `seq` | the identity column's number, assigned here and never handed out again; generates `id` |
| `kind` | `human` \| `group` \| `contact` \| `peer`. Fixed at creation; enforced in Go against `kind.go` |
| `display_name` | required. Display only — never a join key, and not unique |
| `email` | humans, free text, trimmed. **Not unique and never a join key** — two rows may share one |
| `disabled_at` | 0 = active. The only removal; see below |
| `availability` | a human's declared working week in their own zone, `{"tzid","days","start","end"}` — the same shape as kanban-store's board `business_hours`, but the person's, so a Los Angeles board can have an Amsterdam member. **Absent means unknown, and unknown is never available.** Never defaulted. A group never carries one; see [Availability](#availability) |
| `created_at` / `updated_at` | |
| **`acts_in_this_deployment`**, **`acts_as_caller`**, **`restricted_to_its_grants`**, **`holds_service_principal_tokens`** | **computed on read** from the kind, on every record: may hold grants; a request may act as it (so it may be a group member and an administrator); holding no grant of a relation refuses it; it may be issued a token of its own in place of a login. Services read these instead of comparing `kind` to a word. See [Kinds](#kinds) |
| **`groups`** | **computed on read** by `GET /principals/{id}` for a human or a peer: the groups it belongs to. Always present for those, `[]` when none |
| **`members`** | **computed on read** by `GET /principals/{id}` for a group: the humans and peers in it. Always present for a group, `[]` when none |

Exactly one of `groups` / `members` is present, so a reader can tell the kind
from the shape as well as from `kind`. Neither is expanded by `GET /principals`.

### `group_members`

| Field | Meaning |
|---|---|
| `group_id` | a `kind=group` principal. A human here is a 400 |
| `member_id` | a `human`, `peer` or `service` principal (a kind with `acts_as_caller`). A group here is a 400: **no nested groups in v1** |
| `created_at` | |

`(group_id, member_id)` is the primary key and the insert does nothing on a conflict,
so adding the same member twice is idempotent.

### `time_off`

| Field | Meaning |
|---|---|
| `id` | `timeoff_000001`. Nothing joins on it, so this table **is** hard-deleted |
| `principal_id` | a `kind=human` principal. A group here is a 400 |
| `starts_at` / `ends_at` | half-open `[starts_at, ends_at)`, epoch seconds, `starts_at < ends_at`. Overlaps are allowed — two reasons to be away on one day are still one day away |
| `note` | free text, trimmed |
| `created_at` | |

An absence sits beside the week rather than editing it, so a holiday is one
row and not a rewrite of the schedule and a second rewrite to restore it.

### `principal_resources` — gone

Lived here from 2026-09-10 to 2026-09-11 as "what a principal works with", and
moved to **grant-store** (`:8315`) as the advisory `works_with` relation beside
the enforced `can_use`, `can_run_as` and `can_dispatch_on` grants, so there is
one place that says who may use what. The schema drops the table on boot; it
held no rows on any live database when it went. `GET /principals/{id}/resources`
and its `PUT`/`DELETE`, and `GET /resource-types`, answer Go's plain 404 now —
read `GET {grant-store}/principals/{id}/effective?relation=works_with` instead.

---

## Invariants

**`kind` is `human`, `group`, `contact` or `peer`.** Anything else is a **400**
naming the vocabulary. Case is normalized, nothing else is. `GET /kinds` serves
the list so no caller hardcodes it.

**Memberships are one level deep.** `group_id` must be a group and `member_id`
must be a human or a peer, checked in Go on every write and read of a membership because a
`CHECK` cannot look across rows. Putting a group in the member slot is a **400**
that says nested groups are not supported in v1 and to add the group's humans
directly.

**`display_name` is required** on every write path. The name is the only thing
a card or a permission can render, and a row that renders as nothing is
indistinguishable from a broken join.

**There is no hard delete.** No route and no store method removes a row.
`disabled_at` is the only removal: a disabled principal still exists so every
reference from another store keeps resolving. Disabled principals are hidden
from `GET /principals`, `GET /principals/{id}/members`, `GET /principals/{id}/groups`
and the embedded `groups` / `members` unless `include_disabled=true`, and
`GET /principals/{id}` **always** returns them, with `disabled_at` set, because a
card assigned to them still needs to render. `disable` and `enable` are
idempotent: a second `disable` does not move the timestamp.

**`kind` and `disabled_at` do not move through `PATCH`.** `kind` is fixed at
creation (a group that became a human would strand its memberships — create a
new principal). `disabled_at` moves only through `POST /disable` and
`POST /enable`, so removal is always an explicit act. Either key in a `PATCH`
body is a **400** saying so.

**A week without a zone is refused.** `availability.tzid` must be a zone this
host can load — `Europe/Amsterdam`, not `+02:00` — because an offset cannot know
what happens at a DST change and silently drifts the hours twice a year.
`TestAvailabilityIsReadInThePrincipalsOwnZoneAcrossDST` pins it: the same UTC
instant on the Mondays either side of 2026-03-29 is `off_hours` before and
`in_hours` after.

**Ids are prefixed, never bare UUIDs.** dash's resolver probes every registry
row whose id pattern matches, and noteboard already claims the uuid shape, so a
uuid-shaped id here would make every uuid in every chat message probe this store
as well. The prefix also keeps `/` out of the ref, which kanban-store's reverse
lookup splits on.

---


## Contacts

A third kind, beside `human` and `group`: **`contact`** is someone outside this
deployment that work arrives *from* — the requester on a ticket. A contact
never logs in, never holds a grant, never joins a group and is never an
administrator; `ActsInThisDeployment(kind)` is the check, and grant-store and
the login path both ask it.

`POST /contacts/resolve {"email":…,"display_name":…}` turns an address into one
id: **201** with `{"contact":…,"created":true}` the first time that address is
seen, **200** with `created:false` every time after. Matching is on the address
lowercased and trimmed, among active contacts only — so a requester who writes
from their phone in capitals lands on the row their earlier tickets point at,
and an employee who also writes in as a customer is deliberately two rows.

An existing contact **keeps the name it has**: the name on a later mail is not
more true than the one already stored, and overwriting it would rewrite the
requester shown on every one of that person's tickets.

⚠️ This is the only write path in this store that looks a principal up by
anything but its id. That is the point of it: the caller has an address and no
id, and this is where the address becomes one, once.

## Kinds

`GET /kind-definitions` serves each kind with what it may do, and every
principal record carries the same four answers:

| kind | `acts_in_this_deployment` (holds grants) | `acts_as_caller` (makes requests, joins groups, may administer) | `restricted_to_its_grants` | `holds_service_principal_tokens` |
|---|---|---|---|---|
| `human` | yes | yes | no | no |
| `group` | yes | no | no | no |
| `contact` | no | no | no | no |
| `peer` | yes | yes | **yes** | no |
| `service` | yes | yes | **yes** | **yes** |

A **`peer`** is another llm-bridge-server linked to this deployment. It acts
here through a peer token this deployment's llm-bridge-server issued it, never
through a login. It is given access the way a human is — grants, a group for a
role, or `is_administrator` for everything — with one difference: holding no
grant of a relation **refuses** it, where a human holding none is not
restricted. A link that has just been made therefore gives the other server
nothing. A peer has no availability or time off.

A **`service`** is a program on this host that calls other services as
itself, such as orchestratord reading and starting sessions. It never logs in:
it presents a service principal token that llm-bridge-server issued for it,
and an administrator there can revoke the token. Like a peer it is restricted
to its grants, and it has no availability or time off. It is the only kind
with `holds_service_principal_tokens`, which is what llm-bridge-server checks
before it issues or accepts such a token.

## The administrator

A human, a peer or a service may carry `is_administrator`. It is one fact about a person, and the
services that read it — kanban-store, grant-store, llm-bridge-server — let an
administrator past every per-resource check they make: every board, every
grant, every session, granted or not. This store only records it.

- Read it on `GET /principals/{id}` and in every list.
- Set it with `PATCH /principals/{id} {"is_administrator":true}`; a patch that
  does not mention it leaves it alone.
- A **group** or a **contact** is refused it (400): only a kind with
  `acts_as_caller` may carry it.
- It says nothing on its own. A disabled principal is refused before this is
  read, so disabling an administrator removes the access.

## Routes

| Method | Path | Notes |
|---|---|---|
| GET | `/health` | `{"status":"ok","counts":{principals,humans,groups,disabled}}`. Every count is over all rows, disabled included. `principals` counts every kind, so it is `humans + groups` plus the `contact` rows, which have no count of their own |
| GET | `/kinds` | `["human","group","contact","peer"]` |
| GET | `/kind-definitions` | `[KindDefinition]`: each kind with `acts_in_this_deployment`, `acts_as_caller`, `restricted_to_its_grants` and a description |
| GET | `/availability-reasons` | `["in_hours","off_hours","time_off","no_schedule","disabled"]` |
| GET | `/weekday-codes` | `["MO","TU","WE","TH","FR","SA","SU"]` |
| GET | `/principals` | `q` (search, below), `kind`, `include_disabled`, `limit`, `offset`, `available_at` (epoch seconds; present and empty = now) → a **bare array** `[Principal]`, display-name order, memberships not expanded. An unknown `kind` is a **400** naming the vocabulary. `available_at` keeps only the humans available then — it implies `kind=human`, and `kind=group` beside it is a **400** |
| POST | `/principals` | `{kind, display_name, email?}` → **201** with the row |
| GET | `/principals/{id}` | the row plus `groups` (human) or `members` (group). Answers for a disabled principal. `include_disabled` governs whether disabled rows appear in the embedded list |
| PATCH | `/principals/{id}` | `display_name`, `email`, `availability`. `availability` is an object `{tzid, days, start, end}` to replace the week, `{}` or `null` to clear it, absent to leave it alone; a group is a **400**, as is a key inside the object that is not one of the four. `kind` and `disabled_at` are each a **400** naming what does move them; any other key is a **400** naming the three accepted |
| POST | `/principals/{id}/disable` | **200** with the row. Idempotent |
| POST | `/principals/{id}/enable` | **200** with the row. Idempotent |
| GET | `/principals/{id}/members` | group → `[Principal]` of humans. **400** if the id is a human. `include_disabled` as above. With `available_at` (epoch seconds; present and empty = now) only the members available then, and `include_disabled` is ignored — a disabled member is never available. **This is the read kanban-store's assignment pool makes** |
| PUT | `/principals/{group}/members/{member}` | no body. **201** `{"group_id","member_id","created":true}` the first time, **200** with `"created":false` after. **400** if `{group}` is not a group or `{member}` is not a human; **404** if either is missing |
| DELETE | `/principals/{group}/members/{member}` | **204**. **404** if not a member, or if either id is missing |
| GET | `/principals/{id}/groups` | human → `[Principal]` of groups. **400** if the id is a group (a group has no groups: no nesting). `include_disabled` as above |
| GET | `/principals/{id}/availability` | `?at=` epoch seconds, default now → `{"principal_id","at","available","reason"}`. Answers for a disabled human (`reason: disabled`). A group is a **400** pointing at `/members?available_at=`; a non-integer `at` is a **400** |
| GET | `/principals/{id}/time-off` | `[TimeOff]`, soonest first. **400** if the id is a group |
| POST | `/principals/{id}/time-off` | `{starts_at, ends_at, note?}` → **201** with the row. **400** if the range is empty or backwards, or the id is a group |
| DELETE | `/principals/{id}/time-off/{timeOffID}` | **204**. **404** if the row is not that principal's — a row cannot be removed through another principal's path |
| GET | `/identity-providers` | `["entra","discord"]` — the providers a binding can name. `entra`: `tenant_id` and `subject` are GUIDs (`tid`, `oid`). `discord`: `tenant_id` empty, `subject` the Discord user id (a decimal snowflake, never the username) |
| GET | `/identity-bindings` | `principal_id`, `provider`, `tenant_id`, `include_unlinked` → bare array `[IdentityBinding]`, oldest first. Active rows only unless `include_unlinked=true` |
| POST | `/identity-bindings` | `{principal_id, provider, tenant_id, subject, linked_by, note?}` → **201** with the row. **409** if the identity already has an active binding, to anyone, naming it. **400** if the principal is not an active human, `linked_by` is not an active human, or the tenant or subject does not have the provider's shape |
| GET | `/identity-bindings/resolve` | `provider`, `tenant_id`, `subject` → `{"binding","principal"}`. **404** when no active binding exists: the identity is unprovisioned. The principal comes back even when disabled, with `disabled_at` set; the login path refuses it |
| GET | `/identity-bindings/{id}` | the row, active or unlinked. **404** if missing |
| POST | `/identity-bindings/{id}/unlink` | `{unlinked_by, reason}` → **200** with the row. Both fields required. Idempotent: a second unlink returns the row unchanged, keeping the first stamp and reason |

Errors are `{"error":"…"}` and enumerate the valid values, so an agent reading a
400 can retry without guessing. **400** the caller described the record wrongly
(an unknown kind, a blank `display_name`, the wrong kind on either side of a
membership, a nested group, a forbidden or unknown `PATCH` key, a malformed
search query, a week a clock cannot read, a backwards absence, hours or time
off on a group) / **404** no such principal, or no such membership on `DELETE` /
**500** otherwise.

Request bodies are decoded with unknown fields rejected, so a misspelled key is
a 400 rather than a write that silently drops it. `PATCH` reaches the same
answer by a different route: it reads the body as a map so it can name the
forbidden keys, which defeats `DisallowUnknownFields`, so it then checks the
remaining keys against its own allowlist by hand. A `PATCH` of
`{"display_nmae":"…"}` is a 400, not a 200 that changed nothing.

---

## Identity bindings

A company login proves "object X in tenant Y" and nothing more. A row in
`identity_bindings` (`identity_binding_000001`) links that pair — with the
provider, `(provider, tenant_id, subject)` — to one **human** principal. An
administrator writes it; the login never does. For Entra, `tenant_id` is the
`tid` claim and `subject` is the `oid` claim, both GUIDs and stored
lowercased. It is not `sub`, which Entra makes different per application, and
it is never an email.

**A binding is never found by email or display name.** An address is a name,
and an attacker who registers someone's address in another tenant would
otherwise sign in as them. An identity without an active binding is
unprovisioned; the resolve route answers 404 and the login path says so.

One active binding per identity: a check in Go names the existing row, and a
partial unique index (`WHERE unlinked_at = 0`) holds when two links race. A
principal may have more than one binding. No row is deleted: unlink stamps
`unlinked_at`, `unlinked_by` and a required `unlink_reason`, and linking the
identity again writes a new row. `linked_by` and `unlinked_by` must be active
humans. This store has no auth and cannot check that they are administrators;
llm-bridge-server's administrator routes do that and fill the field from the
caller, never from the body.

---

## Search

`q` is matched against a `tsvector` over `display_name` (weight A) and
`email` (weight B) with a GIN index, written with every name and email.
Words split at every space and ASCII punctuation mark, are lower-cased, and
lose their diacritics (José is `jose`), in the text and in the query alike, as
the FTS5 index the standalone store kept read them.

**Bare words are prefix terms.** `q=pri` finds Priya Raman and `q=priya northwind`
finds her by name and email domain together, because the main caller is an
assignee picker reading keystrokes. A bare word with punctuation inside it,
`marcus.feld`, is the phrase `marcus feld` with the last word a prefix.
Punctuation alone, `q=-`, holds no word and matches nothing.

**A query with search syntax means exactly what it says.** A double quote, `*`,
`:`, parentheses, or an upper-case `AND` / `OR` / `NOT` switch a query to it:
`"Dana Example"` (quotes included, URL-encoded) is an exact phrase; a bare word is that whole
word; `word*` or `"a phrase"*` makes the last word a prefix; `display_name:data`
or `email:"northwind eng"` searches one field; `NOT` is binary (`a NOT b`), and
`NOT` binds tighter than `AND` (or two terms side by side), which binds tighter
than `OR`. A phrase never runs from the end of the name into the address. A
phrase can still over-match (`"Data Team"` also finds `Data Team Leads`), so an
exact-name lookup narrows the result by `display_name` on the caller's side.

A malformed query is a **400** naming what is wrong: an unclosed quote or
parenthesis, a dangling operator, a `*` or `:` with nothing before it, a column
other than `display_name` or `email`, a phrase with no word, and the FTS5
syntax that did not survive the move to Postgres — `NEAR`, `{column lists}` and
`^`.

---

## Availability

Added 2026-09-11 so kanban-store can hand a new card to someone who is
actually working, not to whoever the board names as its default. Two parts:

- **The week** — `availability` on the human: `{"tzid":"Europe/Amsterdam",
  "days":["MO","TU","WE","TH","FR"],"start":"09:00","end":"17:00"}`. Day codes
  are RFC 5545 and served at `GET /weekday-codes`; `start` and `end` are
  `HH:MM` in that zone, `start < end`, and the window is half-open (`17:00`
  itself is off hours).
- **Absences** — `time_off` rows, half-open epoch ranges beside the week.

`GET /principals/{id}/availability?at=` reduces both to one answer, and exactly
one reason applies, checked in this order:

| `reason` | `available` | when |
|---|---|---|
| `disabled` | false | `disabled_at` is set, whatever the hours say |
| `no_schedule` | false | the human has no `availability`. **Unknown is never available** — there is no default zone, because a guessed zone makes someone look free at 3am and an assignment made on that guess is worse than none |
| `time_off` | false | a `time_off` row covers `at` |
| `off_hours` | false | outside the week, read in the human's own zone |
| `in_hours` | **true** | inside it |

`GET /principals?available_at=` and `GET /principals/{group}/members?available_at=`
are the same evaluation over a list. Because the week is read in Go, the page
(`limit`, `offset`) is cut **after** the filter, so a page of ten is ten
available humans and not three with seven hiding behind the next offset.

There is no measured presence here — no "last seen", no heartbeat. The first
consumer is the Northwind simulation, whose people are not real, so the
declared week is the whole signal. If presence is ever wanted it is a
separate, written-by-others table, not a change to this one.

---

## Seed data

The standalone store had a seed script for the Northwind demo principals,
written against its own address. It stays with the standalone repo and did not
move here.
