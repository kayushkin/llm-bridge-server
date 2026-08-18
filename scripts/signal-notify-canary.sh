#!/bin/bash
# Live HTTP canary for the structured notification producer,
# POST /sessions/{id}/signals (SESSION-SIGNALS.md, "P4 part 3 as built").
#
# It drives the REAL binary over HTTP rather than a handler in a test, because
# the two things most worth checking here are routing and status codes, and a
# httptest recorder proves neither against the mux the gateway actually serves.
#
# Setup (the DB is seeded before the server boots, so the rows are there when
# it opens the file):
#
#   go build -o /tmp/bridge-notify-test ./cmd/llm-bridge-server
#   sqlite3 /tmp/nc/bridge.db "INSERT INTO sessions (bridge_id,harness,state,type)
#       VALUES ('br_chat','claude_code','idle','interactive'),
#              ('br_worker','claude_code','running','autonomous');"
#   LLMBRIDGE_LISTEN_ADDR=:18777 LLMBRIDGE_DB_PATH=/tmp/nc/bridge.db \
#   LLMBRIDGE_IMAGES_DIR=/tmp/nc/images LLMBRIDGE_BRIDGE_PREFS_PATH=/tmp/nc/prefs.json \
#   LLMBRIDGE_KANBAN_STORE_URL= LLMBRIDGE_SIGNAL_CLASSIFIER_MODEL= /tmp/bridge-notify-test &
#
# The two empty env vars matter: they turn the kanban lookup and the classifier
# off, so every assertion below is about this route and not about a service
# that happens to be up. Poll /health until it answers; do not sleep.
#
# 33 checks. Exits non-zero on the first failure count.
B=http://127.0.0.1:18777
pass=0; fail=0
chk() { # chk <label> <expected> <actual>
  if [ "$2" = "$3" ]; then pass=$((pass+1)); echo "  ok   $1"
  else fail=$((fail+1)); echo "  FAIL $1: expected [$2] got [$3]"; fi
}
post() { curl -s -o /tmp/nc.body -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$2" "$B$1"; }

echo "== attended session mints a chat notification =="
code=$(post /sessions/br_chat/signals '{"title":"The migration finished","body":"12 tables.","severity":"warn"}')
chk "201 Created" 201 "$code"
CHAT_ID=$(jq -r .id /tmp/nc.body)
chk "source=tool"       tool         "$(jq -r .source /tmp/nc.body)"
chk "kind=notification" notification "$(jq -r .kind /tmp/nc.body)"
chk "surface=chat"      chat         "$(jq -r .surface /tmp/nc.body)"
chk "state=open"        open         "$(jq -r .state /tmp/nc.body)"
chk "severity=warn"     warn         "$(jq -r .severity /tmp/nc.body)"
chk "session_type"      interactive  "$(jq -r .session_type /tmp/nc.body)"
chk "no request_id"     null         "$(jq -r '.request_id // "null"' /tmp/nc.body)"

echo "== autonomous worker routes to its kanban card =="
code=$(post /sessions/br_worker/signals '{"title":"blocked on a missing credential"}')
chk "201 Created"       201    "$code"
WORK_ID=$(jq -r .id /tmp/nc.body)
chk "surface=kanban"    kanban "$(jq -r .surface /tmp/nc.body)"
chk "severity defaults" info   "$(jq -r .severity /tmp/nc.body)"
chk "session_type"      autonomous "$(jq -r .session_type /tmp/nc.body)"

echo "== the row is readable through both existing read routes =="
chk "per-session list"  "$WORK_ID" "$(curl -s "$B/sessions/br_worker/signals" | jq -r '.[0].id')"
chk "kanban inbox"      "$WORK_ID" "$(curl -s "$B/signals?state=open&surface=kanban" | jq -r '.[0].id')"
chk "chat inbox"        "$CHAT_ID" "$(curl -s "$B/signals?state=open&surface=chat" | jq -r '.[0].id')"

echo "== refusals =="
code=$(post /sessions/br_chat/signals '{"kind":"question","title":"Ship it?"}')
chk "question is 400"   400 "$code"
grep -q AskUserQuestion /tmp/nc.body && { pass=$((pass+1)); echo "  ok   refusal names AskUserQuestion"; } || { fail=$((fail+1)); echo "  FAIL refusal does not name AskUserQuestion"; }
chk "bad kind 400"      400 "$(post /sessions/br_chat/signals '{"kind":"alert","title":"x"}')"
chk "bad severity 400"  400 "$(post /sessions/br_chat/signals '{"title":"x","severity":"critical"}')"
chk "empty title 400"   400 "$(post /sessions/br_chat/signals '{"body":"no headline"}')"
chk "blank title 400"   400 "$(post /sessions/br_chat/signals '{"title":"   "}')"
chk "bad JSON 400"      400 "$(post /sessions/br_chat/signals '{"title":')"
chk "unknown session"   404 "$(post /sessions/br_nope/signals '{"title":"x"}')"
LONG=$(python3 -c 'import json;print(json.dumps({"title":"t"*201}))')
chk "long title 400"    400 "$(post /sessions/br_chat/signals "$LONG")"
BIG=$(python3 -c 'import json;print(json.dumps({"title":"ok","body":"b"*4001}))')
chk "long body 400"     400 "$(post /sessions/br_chat/signals "$BIG")"
MB=$(python3 -c 'import json;print(json.dumps({"title":"é"*200}))')
chk "200 multibyte ok"  201 "$(post /sessions/br_chat/signals "$MB")"

echo "== a refused call writes nothing =="
chk "only 3 rows exist" 3 "$(curl -s "$B/signals" | jq 'length')"

echo "== the resolve verb closes what this producer raised =="
code=$(curl -s -o /tmp/nc.body -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"state":"acknowledged"}' "$B/signals/$WORK_ID/resolve")
chk "acknowledge 200"   200 "$code"
chk "state acked"       acknowledged "$(jq -r .state /tmp/nc.body)"
chk "resolved_at set"   true "$(jq -r '.resolved_at != null' /tmp/nc.body)"
code=$(curl -s -o /tmp/nc.body -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"state":"acknowledged"}' "$B/signals/$WORK_ID/resolve")
chk "second click 200"  200 "$code"
chk "gone from inbox"   0   "$(curl -s "$B/signals?state=open&surface=kanban" | jq 'length')"
chk "dismiss the other" 200 "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"state":"dismissed"}' "$B/signals/$CHAT_ID/resolve")"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
