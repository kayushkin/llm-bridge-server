# prediction-store — API contract

The calibration ledger: resolvable claims, the odds stated for them, and what
actually happened.

Package `internal/tasktracking/predictions` of llm-bridge-server, mounted
by the server under `/prediction-store/`: every route below is served behind
that prefix (`GET /prediction-store/calibration`). The routes are operator
routes (`routeOperatorOnly`): the service token, an administrator, or the
session agent token of an administrator's session. The rows have no owning
principal, so any caller could rewrite or purge anyone's prediction; that
changes when they get one.

The records are in Postgres, in schema `predictions` acting as role
`predictions_module` (`postgresstorage`), built by the `.sql` files in
`migrations/`. The server provisions, migrates and opens the module at start
when `LLMBRIDGE_POSTGRES_URL` is set; without it the routes answer 503
`prediction_store_off`. All timestamps are epoch seconds (`bigint`, 0 =
unset). Tags are a `jsonb` array.

`llm-bridge-server import-prediction-store -sqlite <file>` copies a standalone
prediction-store's SQLite file in, keeping every number, id, time and
soft-deleted row, into an empty module only, and numbers on after the highest
it copied.

---

## The three tables

### `predictions` — the claim and the odds on it

| Field | Meaning |
|---|---|
| `id` | `prediction_000142`: `prediction_` and `seq`, written by `postgresstorage.FormatRecordID`. Not stored; only the exact form names a row |
| `seq` | the identity column. Postgres hands each number out once, so a purged number is never reused; links and estimates join on it |
| `claim` | what you are asserting, in one sentence. Required |
| `resolution_criteria` | what will settle it, and how. Required |
| `category` | the kind of claim — one of the vocabulary below. A synonym resolves; anything else is a 400 |
| `tags` | JSON array, blank entries dropped |
| `provenance` | where the belief came from, not how strong it is. Moves to the newest estimate's provenance |
| `author` | who or what stated it. `(unattributed)` when grouping if empty |
| `probability` | the **current** odds: denormalized from the newest row in `estimates` |
| `initial_probability` | the opening odds, frozen at creation. Never moves, so the gap to `probability` measures how well you update rather than how well you guess |
| `due_at` | resolve-by. 0 = no deadline; only a dated open row can be overdue. A date that has **already passed** is a **400** on both `POST` and `PATCH` — see below |
| `status` | `open` \| `resolved` \| `void` |
| `outcome` | `true` \| `false`. Empty while open, and empty on a `void` row — void is not a miss |
| `resolved_at` | when the outcome was recorded |
| `resolution_note` | why it resolved that way. Searchable. The only outcome-shaped field `PATCH` accepts, and only after the row resolved — it explains a result it cannot change. Refused at creation |
| `created_at` / `updated_at` | |
| `deleted_at` | soft delete: 0 = live. Hidden from every read path but `?include_deleted` |
| **`estimates`** | **computed on read** — the full trail, oldest first. Ignored on write |
| **`links`** | **computed on read.** The one exception: a `links` array in a `POST /predictions` body is inserted inside the create transaction. Ignored by every other write |
| **`brier`** | **computed on read**, never stored. `(probability - outcome)²`, present only on a row resolved `true` or `false`. Ignored on write |

### `estimates` — every revision of the number

| Field | Meaning |
|---|---|
| `id` | integer, an identity column |
| `prediction_id` | cascades on purge |
| `probability` | the odds as of this revision, on the ladder |
| `rationale` | what moved you. The opening row is written with `initial estimate` |
| `provenance` | defaults to the prediction's when omitted |
| `author` | defaults to the prediction's when omitted |
| `created_at` | ordering key, then `id` |

Append-only. There is no update route and no delete route: rewriting your old
odds is how a calibration record becomes a flattering one.

### `links` — what the prediction is about

| Field | Meaning |
|---|---|
| `id` | integer, an identity column. `DELETE /links/{id}` takes this, not the prediction id |
| `prediction_id` | cascades on purge |
| `entity_type` | free text, a type from reference-store's list of reference types (`GET /types`). **Not validated here** — kanban-store does not validate it either, and a hardcoded copy of somebody else's vocabulary goes stale silently |
| `entity_ref` | the id in the store that owns the record |
| `label` | display only, never read back to find the row |
| `created_at` | |

`(prediction_id, entity_type, entity_ref)` is unique and the insert is
`ON CONFLICT DO NOTHING`, so re-linking the same entity is idempotent — the same
uniqueness kanban-store uses.

---

## Invariants

**Probability moves only through `POST /predictions/{id}/estimates`.** The old
number is never overwritten, so the record shows what you believed and when it
changed. `PATCH` with a `probability` key is a 400 naming this route.

**Outcome moves only through `POST /predictions/{id}/resolve`.** It happens
once — a second call is a **409** quoting the outcome already stored and when.
Adding an estimate to a non-open row is a 409 for the same reason: revising the
odds after the answer is known is not an update, it is a rewrite.

**`resolution_criteria` is required on every write path.** A claim that cannot
resolve cannot be scored, and an unresolvable row is worse than no row.

**Probabilities must sit on the ladder.** Off-ladder is a 400 naming the two
nearest rungs. `0.73` claims a precision nobody has, and refusing it keeps the
Murphy decomposition exact. 0 and 1 are refused too: they are not odds you can
be wrong about, and they make the log score infinite.

**Ids are prefixed, never bare UUIDs.** dash's resolver probes every registry
row whose id pattern matches, and noteboard already claims the uuid shape, so a
uuid-shaped id here would make every uuid in every chat message probe this store
as well. The prefix also keeps `/` out of the ref, which kanban-store's reverse
lookup splits on.

---

## Writing a resolution criterion

The store enforces that a criterion exists. It cannot enforce that the criterion
works, and a criterion that does not work is the main way rows die: they sit
open, score nothing, and quietly hollow out every number below.

One test decides it. **Could you run this yourself, today, and would it give the
same answer whoever ran it?** Write the criterion as the command you would run
and the reading that settles it:

```
sqlite3 ~/.config/model-store/store.db "select short_name from models
where id='claude-fable-5-1'" on or after 2026-09-04; non-empty resolves true
```

Three ways it goes wrong, each taken from a row in this store:

**It waits on somebody else's behavior.** `prediction_000004` claimed that
restoring an nginx route fixes a Discord bot, and asked for "a new row in
`transitions` with `actor=user`". The route was fixed and answered nine live
interactions; the row stayed unresolvable because nobody had clicked Join.
The claim was about a route, the criterion was about a stranger. Ask for the
thing you changed, not for the reaction you hope it causes.

**There is no command in it.** `prediction_000009` resolves when consolidating a
policy "eliminates user reports of missing final responses over 3 weeks". There
is nothing to run, so nothing will run it. A criterion with no executable step
is a wish with a deadline attached.

**It answers the same either way.** A check that passes under every explanation
you are weighing measures none of them. Spend the criterion on the cheapest
check that would come out *differently* if you are wrong.

Two more rules that cost nothing and are skipped anyway:

**Say what makes it false.** A criterion with only a success condition never
resolves false — it stays open until someone gives up on it, and a ledger of
abandoned rows scores better than it deserves. `non-empty resolves true` above
implies the other branch; write it out when it does not.

**Set `due_at`.** Only a dated open row can be overdue, and only an overdue row
reaches `scripts/prediction-resolve-sweep.sh` and the todo it maintains. An
undated row is invisible to the single mechanism that will ever remind you it
exists. If you genuinely cannot date the answer, date the next time you will
*look* — a row you review and re-date has cost one minute; a row nobody ever
looks at cost the whole prediction.

**A deadline already in the past is refused.** `POST /predictions` and
`PATCH /predictions/{id}` both answer **400** for a `due_at` that is not in the
future, naming the date they read back. Nothing can be predicted about a moment
that has gone, and the mistake is silent otherwise: every read path treats such
a row as merely *late*, so the overdue sweep reports it and the todo it
maintains tells you to resolve a claim whose evidence window has not opened.
Measured 2026-09-04 — three of twenty-nine live rows carried a `due_at` a year
before their own `created_at`, and two were being reported as "336 days overdue"
and "307 days overdue".

`PATCH` checks only a deadline that **moves**. A row goes overdue by the clock
advancing, not by anyone writing to it, so a read-modify-write caller resending
the deadline it just read can still correct the claim, and a wrong date can
always be corrected *forwards*.

---

## Vocabularies

`GET /vocabulary` (and its alias `GET /categories`) serves all six lists, so no
caller builds a filter from whatever values the rows happen to hold.

- **categories** — the kind of claim, chosen so a slice by category answers
  "what am I systematically wrong about": `root-cause`, `will-fix`, `behavior`,
  `will-ship-by`, `effort`, `will-recur`, `external`. A synonym resolves
  (`diagnosis` → `root-cause`, `eta` → `will-ship-by`, `flaky` → `will-recur`);
  anything else is a 400 naming the whole list. Keep the list short — a category
  with three predictions in it scores nothing.
- **provenances** — `measured`, `read`, `inferred`, `guessed`. Where the belief
  came from is a different fact from how strong it is: 90% from a live call and
  90% from a plausible-looking README should not be spent the same way.
- **statuses** — `open`, `resolved`, `void`. `void` means the claim stopped
  being resolvable (the work was canceled, the question changed) and is
  excluded from every score rather than counted as a miss.
- **outcomes** — `true`, `false`, `void`, accepted by `POST /resolve`. `void`
  stores `status=void` and leaves `outcome` empty.
- **group_by** — `category`, `provenance`, `author`, `tag`, `entity_type`,
  `lead_time`, `probability`. Anything else is a 400. `lead_time` buckets
  `resolved_at - created_at` into `under-1h`, `under-1d`, `under-1w`,
  `under-1mo`, `over-1mo`.
- **probability_ladder** — 23 rungs: `0.001`, `0.01`, then every 0.05 from
  `0.05` to `0.95`, then `0.99`, `0.999`. Symmetric about 0.5, so a claim you
  think unlikely gets the same resolution as one you think likely.

---

## Routes

Each path is behind `/prediction-store`.

| Method | Path | Notes |
|---|---|---|
| GET | `/health` | `{"status":"ok","counts":{predictions,open,overdue}}` |
| GET | `/vocabulary` | `{categories, provenances, statuses, outcomes, group_by, probability_ladder}` |
| GET | `/categories` | the same payload, same handler |
| GET | `/tags` | `{"tags":[{tag,count}]}` over live predictions, most used first |
| GET | `/calibration` | every listing filter below, plus `group_by`. Groups sorted worst Brier first |
| GET | `/base-rates` | same filters; `group_by` defaults to `category`. Groups sorted by `n` descending |
| GET | `/predictions` | `status`, `category`, `tag`, `author`, `provenance`, `outcome`, `entity_type`, `entity_ref`, `q`, `overdue` (alias `due`), `since`, `until`, `include_deleted`, `expand`, `limit`, `offset` → `{"predictions":[…],"total":n}`, where `total` ignores limit/offset. Newest first. `expand=1` attaches estimates and links to each row |
| POST | `/predictions` | **201** with the created row. Writes the opening estimate in the same transaction, and any `links` in the body |
| GET | `/predictions/{id}` | with estimates and links. Readable when soft-deleted |
| PATCH | `/predictions/{id}` | `claim`, `resolution_criteria`, `category`, `tags`, `provenance`, `author`, `due_at`, `resolution_note`. `probability`, `outcome` and `status` are each a **400** naming the route that does move them. `resolution_note` is a **400** on a row that has not resolved yet. A `due_at` that *changes* to a date already past is a **400**; resending the current one is not |
| DELETE | `/predictions/{id}` | soft → `{"deleted":id}`; `?hard=true` → `{"purged":id}`, taking its estimates and links by cascade |
| POST | `/predictions/{id}/restore` | undoes a soft delete; returns the row |
| GET | `/predictions/{id}/estimates` | `{"estimates":[…]}`, oldest first |
| POST | `/predictions/{id}/estimates` | `{probability, rationale, provenance, author}` → **201** with the whole prediction. **409** unless the row is open |
| POST | `/predictions/{id}/resolve` | `{outcome, note}` → **200** with the row. **409** if already resolved or void |
| GET | `/predictions/{id}/links` | `{"links":[…]}` |
| POST | `/predictions/{id}/links` | `{entity_type, entity_ref, label}` → **201**. Idempotent; both refs required |
| DELETE | `/links/{id}` | numeric link id → `{"deleted":id}`; a non-numeric id is a **400** |

Errors are `{"error":"…"}` and enumerate the valid values, so an agent reading a
400 can retry without guessing. **400** the caller described the record wrongly
(including an off-ladder probability, an unknown category, provenance, outcome
or `group_by`, and a malformed search query) / **404** no such row / **409**
already resolved, or an estimate on a row that is not open / **500** otherwise.

Request bodies are decoded with unknown fields rejected, so a misspelled key is
a 400 rather than a write that silently drops it. `PATCH` reaches the same
answer by a different route: it reads the body as a map so it can name the three
forbidden keys, which defeats `DisallowUnknownFields`, so it then checks the
remaining keys against its own allowlist by hand. A `PATCH` of `{"clam":"…"}` is
a 400, not a 200 that changed nothing.

---

## Scoring

`GET /calibration` returns an overall `Score` and, with `group_by`, one per
group. All of it runs on resolved rows with an outcome of `true` or `false`;
`open` and `void` are counted separately and excluded from the arithmetic.

- **Brier** — mean of `(probability - outcome)²`. 0 is perfect, 0.25 is what
  always saying 50% gets you. Lower is better.
- **Log score** — mean of `log(p)` when it came true and `log(1-p)` when it did
  not. Closer to 0 is better; it punishes confident misses far harder than
  Brier does.
- **The overconfidence view** — `mean_probability` against `observed_rate` is
  the raw comparison, and it misreads a correct low-probability call: a claim
  stated at 20% that came out false was a *correct* call made with 80%
  confidence, and the raw comparison scores it as a miss. So `mean_confidence`
  is the mean of `max(p, 1-p)`, `accuracy` is how often the side you leaned on
  won, and `overconfidence` is `mean_confidence - accuracy` — the gap that
  actually needs correcting.
- **Murphy's decomposition** — `Brier = reliability - resolution + uncertainty`.
  The two terms need opposite corrections. Bad **reliability** means your
  numbers are shifted: shade them. Bad **resolution** means you are not
  discriminating at all — your 90%s and your 60%s come true equally often — and
  no amount of shading fixes that; stop dressing guesses as estimates.
  `uncertainty` is `base_rate × (1 - base_rate)`, the difficulty of the set
  rather than anything about you. The identity is **exact** here because the
  decomposition groups on the exact stated probability, which the ladder makes
  possible — with free-form probabilities it would be an artifact of where the
  bucket edges fell.
- **skill_score** — `1 - brier/uncertainty`: how much better than always
  guessing the base rate. Positive is skill, 0 is no better than the base rate,
  negative is worse. **Null** when every outcome was the same, because the
  comparison is undefined then, not zero.
- **brier_initial / update_gain / revised_count** — `brier_initial` scores the
  opening number, `brier` the final one, and `update_gain` is the difference.
  Positive means revising on evidence moved you toward the truth.
  `revised_count` is how many rows have more than the opening estimate.
- **buckets** — the calibration curve in deciles of the stated probability:
  `n`, `mean_probability`, `observed_rate` and their `gap` (positive = too
  confident in that decile). Empty deciles are omitted.

`GET /base-rates` is the smaller answer: per group, `n`, `true`, `false` and
`rate`. That is the number "start from the base rate" needs and could not
previously get.

---

## Search

`q` is read as a web search (`websearch_to_tsquery`, simple configuration: no
stemming, no stop words, case folded) over `claim`, `resolution_criteria`,
`tags` and `resolution_note`: every word must appear, `"quoted words"` must
appear together, `or` between two words lets either match, and `-word` must not
appear. Every ASCII punctuation mark separates words, in the text and in the
query, as the FTS5 tokenizer this store used before did, so `deploy` finds
`deploy.sh`, and `deploy.sh` is the phrase `"deploy sh"`. An unclosed quote, or
a query with no word in it, is a **400** naming the query. FTS5's prefix
(`word*`), `NEAR` and column filters are gone; `NOT` is `-`.

The search document is a generated `tsvector` column with a GIN index, so it
follows every write without triggers. On a copy of the live ledger taken
2026-11-23, thirteen one-word queries found exactly the rows FTS5 found.
