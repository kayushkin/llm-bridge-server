# prediction-store

The calibration ledger, mounted in llm-bridge-server at `/predictions/`:
resolvable claims (`predictions`), every revision of the odds on them
(`estimates`), and what they are about (`links`).

One record type per table, ids handed out here and joined on everywhere else.
It was a service of its own on `:8313` until it moved into the bridge's
`task_tracking` group.

## Why it exists

It makes one line of the house directives measurable. "Start from the base rate"
is unfollowable without a record of how often claims of a kind turn out true,
and until this store existed that number was always invented.
`GET /base-rates` answers it from resolved rows.

## Run it

It runs inside llm-bridge-server, from Postgres, when `LLMBRIDGE_POSTGRES_URL`
is set; `CONTRACT.md` says how. Its tests need a Postgres:

```bash
export POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh start)"   # from the repo root
go test ./internal/tasktracking/predictions/
```

## Use it

```bash
BRIDGE=http://localhost:8160
AUTH="Authorization: Bearer $LLM_BRIDGE_PRINCIPAL_TOKEN"   # an administrator's session

# state a claim — resolution_criteria, category, provenance and a ladder
# probability are all required
curl -s -X POST "$BRIDGE/prediction-store/predictions" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"claim":"The 502s are auth-store token refresh, not nginx",
       "resolution_criteria":"journalctl -u auth-store --since 14:00 | grep refresh, against the 502 timestamps in /var/log/nginx/error.log. Resolves true if every 502 sits inside a refresh window, false if any 502 falls outside one",
       "category":"root-cause","provenance":"inferred","probability":0.6,
       "due_at":1788400000,"tags":["ops"],"author":"claude"}'

# revise it — the old number stays, this appends
curl -s -X POST "$BRIDGE/prediction-store/predictions/prediction_000001/estimates" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"probability":0.9,"rationale":"refresh failures at 14:02 and 14:31","provenance":"measured"}'

# record what happened — once; a second call is a 409
curl -s -X POST "$BRIDGE/prediction-store/predictions/prediction_000001/resolve" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"outcome":"true","note":"token TTL was 300s"}'

curl -s -H "$AUTH" "$BRIDGE/prediction-store/calibration?group_by=category"   # what am I wrong about
curl -s -H "$AUTH" "$BRIDGE/prediction-store/base-rates"                      # how often this kind comes true
curl -s -H "$AUTH" "$BRIDGE/prediction-store/predictions?status=open&overdue=1"
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
