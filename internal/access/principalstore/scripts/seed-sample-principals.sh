#!/usr/bin/env bash
# seed-sample-principals: create the operator and the sample humans and groups
# the other demo data on this box already refers to, so principals line up with
# names that exist in kanban's card_events.actor.
#
# It also gives the four Northwind demo humans a working week in staggered zones,
# puts one of them on time off for the current week, and puts all four in a
# "Northwind Eng" group, so a kanban board whose assignment_pool names that group
# has someone available at most hours and someone to skip. The operator gets no
# week: the operator is a real person, and a declared week would make them look available
# to the demo.
#
# Idempotent. Each principal is looked up by an exact display_name and kind
# before it is created; a second run finds them all and creates nothing. If two
# rows ever match one lookup the script exits non-zero instead of guessing,
# because a guess here would seed memberships onto the wrong row.
#
# Prints one line per principal, created or found:
#   principal_000001  human  Slava Kayushkin
set -euo pipefail

STORE="${PRINCIPAL_STORE_URL:-http://127.0.0.1:8314}"

if ! curl -sfS "$STORE/health" >/dev/null; then
  echo "seed-sample-principals: cannot reach principal-store at $STORE" >&2
  exit 1
fi

SEED_STORE="$STORE" python3 <<'PYTHON'
import datetime
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
import zoneinfo

store = os.environ["SEED_STORE"]

# Slava is the operator and is real. The other four are the Northwind demo team
# already present in kanban's card_events.actor, so a card seeded there can be
# assigned to a principal that exists.
humans = [
    ("Slava Kayushkin", "slava@kayushkin.com"),
    ("Priya Raman", "priya.raman@northwind-eng.example"),
    ("Marcus Feld", "marcus.feld@northwind-eng.example"),
    ("Dinesh Okonkwo", "dinesh.okonkwo@northwind-eng.example"),
    ("Helena Vos", "helena.vos@northwind-eng.example"),
]
groups = ["Data Team", "Security", "Northwind Eng"]
memberships = {
    "Data Team": ["Priya Raman", "Dinesh Okonkwo", "Slava Kayushkin"],
    "Security": ["Helena Vos", "Marcus Feld", "Slava Kayushkin"],
    "Northwind Eng": ["Priya Raman", "Marcus Feld", "Dinesh Okonkwo", "Helena Vos"],
}

# Each demo human's working week in their own zone. Three zones spread the
# team across the clock, so a pool on "Northwind Eng" has someone in hours for
# most of a weekday and nobody at a weekend.
weekdays = ["MO", "TU", "WE", "TH", "FR"]
working_weeks = {
    "Priya Raman": "America/Los_Angeles",
    "Marcus Feld": "America/Los_Angeles",
    "Dinesh Okonkwo": "Asia/Kolkata",
    "Helena Vos": "Europe/Amsterdam",
}
# Away for the whole of the week the script runs in, Monday to Monday in their
# own zone. A later run in a later week adds that week's row too.
on_time_off_this_week = "Marcus Feld"


def request(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(store + path, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as err:
        raw = err.read().decode(errors="replace")
        print("seed-sample-principals: %s %s -> %d %s" % (method, path, err.code, raw), file=sys.stderr)
        sys.exit(1)


def find_or_create(kind, display_name, email=""):
    # An exact phrase query, quoted so the store passes it through as FTS5
    # syntax rather than turning each word into a prefix term. A phrase can
    # still over-match ("Data Team" also finds "Data Team Leads"), so the
    # result is narrowed to the exact display_name here.
    query = urllib.parse.urlencode({
        "q": '"%s"' % display_name.replace('"', '""'),
        "kind": kind,
        "include_disabled": "true",
    })
    _, rows = request("GET", "/principals?" + query)
    exact = [r for r in rows if r["display_name"] == display_name]
    if len(exact) > 1:
        print(
            "seed-sample-principals: %d %s rows are named %r (%s) — refusing to guess"
            % (len(exact), kind, display_name, ", ".join(r["id"] for r in exact)),
            file=sys.stderr,
        )
        sys.exit(1)
    if exact:
        return exact[0]
    # A human whose address another row already carries has usually been
    # renamed in the store, not gone missing. Creating one here would seed a
    # second copy and put it in the groups below, so stop and name the row.
    if email:
        query = urllib.parse.urlencode({
            "q": '"%s"' % email.replace('"', '""'), "kind": kind, "include_disabled": "true",
        })
        _, rows = request("GET", "/principals?" + query)
        same_email = [r for r in rows if r["email"] == email]
        if same_email:
            print(
                "seed-sample-principals: no %s is named %r, but %s is already on %s — "
                "renamed in the store? Change the name in this script rather than create a copy"
                % (kind, display_name, email, ", ".join("%s (%r)" % (r["id"], r["display_name"]) for r in same_email)),
                file=sys.stderr,
            )
            sys.exit(1)
    status, created = request("POST", "/principals", {
        "kind": kind, "display_name": display_name, "email": email,
    })
    if status != 201:
        print("seed-sample-principals: POST /principals answered %d" % status, file=sys.stderr)
        sys.exit(1)
    return created


by_name = {}
for name, email in humans:
    by_name[name] = find_or_create("human", name, email)
for name in groups:
    by_name[name] = find_or_create("group", name)

for group, members in memberships.items():
    for member in members:
        status, _ = request(
            "PUT", "/principals/%s/members/%s" % (by_name[group]["id"], by_name[member]["id"]))
        if status not in (200, 201):
            print("seed-sample-principals: PUT member answered %d" % status, file=sys.stderr)
            sys.exit(1)

for name, tzid in working_weeks.items():
    week = {"tzid": tzid, "days": weekdays, "start": "09:00", "end": "17:00"}
    request("PATCH", "/principals/%s" % by_name[name]["id"], {"availability": week})

away_id = by_name[on_time_off_this_week]["id"]
zone = zoneinfo.ZoneInfo(working_weeks[on_time_off_this_week])
today = datetime.datetime.now(zone).date()
monday = today - datetime.timedelta(days=today.weekday())
starts_at = int(datetime.datetime.combine(monday, datetime.time(), zone).timestamp())
ends_at = int(datetime.datetime.combine(
    monday + datetime.timedelta(days=7), datetime.time(), zone).timestamp())
_, existing = request("GET", "/principals/%s/time-off" % away_id)
if not any(r["starts_at"] == starts_at and r["ends_at"] == ends_at for r in existing):
    request("POST", "/principals/%s/time-off" % away_id, {
        "starts_at": starts_at, "ends_at": ends_at, "note": "seed-sample-principals: away this week",
    })

for name, _ in humans:
    p = by_name[name]
    print("%s  %s  %s" % (p["id"], p["kind"], p["display_name"]))
for name in groups:
    p = by_name[name]
    print("%s  %s  %s" % (p["id"], p["kind"], p["display_name"]))
PYTHON
