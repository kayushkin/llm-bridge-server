# prediction-store

`:8313`. The calibration ledger: resolvable claims (`predictions`), every
revision of the odds on them (`estimates`), and what they are about (`links`).

Part of the store family alongside event-store `:8308`, quote-store `:8309` and
job-store `:8311`, and the same shape as all three: one record type per table,
ids handed out here and joined on everywhere else, routes rooted at `/`.

## Why it exists

It makes one line of the house directives measurable. "Start from the base rate"
is unfollowable without a record of how often claims of a kind turn out true,
and until this store existed that number was always invented.
`GET /base-rates` answers it from resolved rows.

## Run it

```bash
./deploy.sh          # test, build, install the unit, start it, smoke-check
make check           # fmt, vet, test, build
```

The `sqlite_fts5` build tag is mandatory: a binary built without it starts and
then dies applying the schema with `no such module: fts5`. `Makefile` and
`deploy.sh` both carry it, and `Open()` names it if a binary from elsewhere
shows up.

Env: `PREDICTION_STORE_ADDR` (default `:8313`), `PREDICTION_STORE_DATA_DIR`
(default `~/.config/prediction-store`). SQLite at
`<data dir>/prediction-store.db`, WAL.

## Use it

```bash
# state a claim — resolution_criteria, category, provenance and a ladder
# probability are all required
curl -s -X POST http://localhost:8313/predictions \
  -H 'Content-Type: application/json' \
  -d '{"claim":"The 502s are auth-store token refresh, not nginx",
       "resolution_criteria":"journalctl -u auth-store --since 14:00 | grep refresh, against the 502 timestamps in /var/log/nginx/error.log. Resolves true if every 502 sits inside a refresh window, false if any 502 falls outside one",
       "category":"root-cause","provenance":"inferred","probability":0.6,
       "due_at":1788400000,"tags":["ops"],"author":"claude"}'

# revise it — the old number stays, this appends
curl -s -X POST http://localhost:8313/predictions/prediction_000001/estimates \
  -H 'Content-Type: application/json' \
  -d '{"probability":0.9,"rationale":"refresh failures at 14:02 and 14:31","provenance":"measured"}'

# record what happened — once; a second call is a 409
curl -s -X POST http://localhost:8313/predictions/prediction_000001/resolve \
  -H 'Content-Type: application/json' \
  -d '{"outcome":"true","note":"token TTL was 300s"}'

curl -s "http://localhost:8313/calibration?group_by=category"   # what am I wrong about
curl -s "http://localhost:8313/base-rates"                      # how often this kind comes true
curl -s "http://localhost:8313/predictions?status=open&overdue=1"
```

The criterion above is written as the two commands that settle it and the
reading that decides each outcome, and it carries a `due_at`. Both are
deliberate, and both are the usual reason a row dies: nothing to run means
nobody runs it, and an undated row can never be overdue, so the overdue sweep
never surfaces it. `CONTRACT.md` has the rubric under **Writing a resolution
criterion**.

`CONTRACT.md` is the route table and the field-by-field reference.

## Design notes

**A note can be corrected; an outcome cannot.** `resolution_note` is the one
outcome-shaped field `PATCH` accepts, and only on a row that has already
resolved — the note explains a result it has no power to change, so fixing
hurried prose costs the ledger nothing. On an open row it is a 400 naming
`POST /resolve`, and at creation it is a 400 rather than the silent discard it
used to be.

**The estimates trail is append-only.** There is no update route and no delete
route for an estimate. `probability` on the prediction is only ever the newest
one, and `initial_probability` is frozen at creation, so the ledger can score
how well you *update* and not just how well you guess. Rewriting your old odds
is how a calibration record becomes a flattering one.

**Probabilities sit on a ladder** — 0.001, 0.01, every 0.05 up to 0.95, then
0.99 and 0.999. Anything else is a 400 naming the two nearest rungs. `0.73`
claims a precision nobody has, and quantising also makes the Murphy
decomposition exact rather than an artifact of where the bucket edges fell.

## License

MIT — see [LICENSE](LICENSE).
