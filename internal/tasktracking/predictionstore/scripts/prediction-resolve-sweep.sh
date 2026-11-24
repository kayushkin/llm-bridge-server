#!/usr/bin/env bash
# prediction-resolve-sweep: keep ONE noteboard todo listing the open predictions
# whose due date has passed. Runs as a scheduler shell job.
#
# Why it exists: a calibration ledger scores resolved rows and nothing else. An
# open row past its due date scores nothing, so predictions logged and never
# resolved quietly hollow out every number the store computes. Something has to
# put them back in front of the user.
#
# This script does NOT remind anyone. It sends no message, opens no session and
# creates no job. ~/bin/reminder-coordinator (scheduler job 32) is the only thing
# on this box that nudges the user, and it delivers this todo through the todo's
# own schedule.remind.nag rule. Never add a cron job that nudges about a todo —
# put the cadence on the todo and let the coordinator deliver it.
set -euo pipefail

# Defaults are the documented ports: prediction-store :8313, noteboard :8191.
# Override either to point at a test instance.
STORE="${PREDICTION_STORE_URL:-http://localhost:8313}"
NOTEBOARD="${NOTEBOARD_URL:-http://localhost:8191}"

# The exact title of the one row this script owns. It is the join key: change it
# here and the existing todo is orphaned and a second one appears.
TODO_TITLE="⚖️ Overdue predictions — resolve these"

# The two responses go to files, not to variables. A noteboard search result
# carries every matching item's full body and routinely runs past the kernel's
# per-argument limit, which turns handing it to python via the environment into
# "Argument list too long" — an error about argv, in a script about predictions.
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

if ! curl -sfS "$STORE/predictions?overdue=1&limit=100" -o "$WORK_DIR/overdue.json"; then
  echo "prediction-resolve-sweep: cannot reach prediction-store at $STORE — no sweep this run" >&2
  exit 1
fi

# Search instead of listing every item: this todo is one row among hundreds.
# include_held=true because a held copy still exists and still counts — a lookup
# that cannot see it creates a second one on the next tick.
if ! curl -sfS -G "$NOTEBOARD/api/search" \
    --data-urlencode "q=overdue predictions resolve" \
    --data-urlencode "type=todo" \
    --data-urlencode "include_held=true" \
    -o "$WORK_DIR/found.json"; then
  echo "prediction-resolve-sweep: cannot reach noteboard at $NOTEBOARD — no sweep this run" >&2
  exit 1
fi

SWEEP_STORE="$STORE" \
SWEEP_NOTEBOARD="$NOTEBOARD" \
SWEEP_TITLE="$TODO_TITLE" \
SWEEP_OVERDUE_FILE="$WORK_DIR/overdue.json" \
SWEEP_FOUND_FILE="$WORK_DIR/found.json" \
python3 <<'PYTHON'
import datetime
import json
import os
import sys
import time
import urllib.error
import urllib.request
from zoneinfo import ZoneInfo

store = os.environ["SWEEP_STORE"]
noteboard = os.environ["SWEEP_NOTEBOARD"]
title = os.environ["SWEEP_TITLE"]

def load(env_var, what):
    raw = open(os.environ[env_var], encoding="utf-8").read()
    try:
        return json.loads(raw)
    except ValueError as err:
        print(
            "prediction-resolve-sweep: %s returned a payload this script cannot "
            "read: %s (%s)" % (what, raw[:300], err),
            file=sys.stderr,
        )
        sys.exit(1)


predictions = load("SWEEP_OVERDUE_FILE", "prediction-store").get("predictions") or []
candidates = load("SWEEP_FOUND_FILE", "noteboard")

# Match the title exactly. Full-text search is a prefilter, not the answer: it
# also returns rows that merely mention these words.
existing = [item for item in candidates if item.get("title") == title]
if len(existing) > 1:
    ids = ", ".join(item["id"] for item in existing)
    print(
        "prediction-resolve-sweep: %d todos share the title %r (%s). This script "
        "owns exactly one. Merge them by hand, then re-run."
        % (len(existing), title, ids),
        file=sys.stderr,
    )
    sys.exit(1)
todo = existing[0] if existing else None


def call(method, url, payload=None):
    """One HTTP call. Raises with the server's own message rather than a bare code."""
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read().decode())
    except urllib.error.HTTPError as err:
        body = err.read().decode(errors="replace")[:500]
        raise SystemExit(
            "prediction-resolve-sweep: %s %s failed with %d: %s"
            % (method, url, err.code, body)
        )
    except urllib.error.URLError as err:
        raise SystemExit(
            "prediction-resolve-sweep: %s %s did not answer: %s" % (method, url, err.reason)
        )


def overdue_days(prediction, now):
    return max(0, (now - int(prediction.get("due_at") or 0)) // 86400)


def overdue_phrase(days):
    if days == 0:
        return "due earlier today"
    if days == 1:
        return "1 day overdue"
    return "%d days overdue" % days


def stamp(epoch):
    return time.strftime("%Y-%m-%d", time.localtime(epoch))


def build_body(predictions, now):
    """The markdown the user reads. Ids appear verbatim so the chat surfaces
    turn them into reference chips."""
    lines = [
        "Each of these is open and past its due date. Resolve it, or void it if "
        "it stopped being resolvable. Unresolved rows score nothing.",
        "",
        "%d overdue. Swept %s." % (len(predictions), stamp(now)),
    ]
    for prediction in sorted(predictions, key=lambda p: int(p.get("due_at") or 0)):
        identifier = prediction["id"]
        days = overdue_days(prediction, now)
        lines += [
            "",
            "---",
            "",
            "### %s — %s" % (identifier, overdue_phrase(days)),
            "",
            prediction.get("claim") or "(no claim recorded)",
            "",
            "- Stated probability: %s" % prediction.get("probability"),
            "- Due: %s" % stamp(int(prediction.get("due_at") or 0)),
            "- Category: %s" % (prediction.get("category") or "(none)"),
            "- Resolves when: %s" % (prediction.get("resolution_criteria") or "(none recorded)"),
            "",
            "```bash",
            "curl -s -X POST %s/predictions/%s/resolve \\" % (store, identifier),
            "  -H 'Content-Type: application/json' \\",
            """  -d '{"outcome":"true","note":"what happened"}'""",
            "```",
        ]
    lines += [
        "",
        "---",
        "",
        "`outcome` is `true`, `false` or `void`. Resolving is one-way: a second "
        "call returns 409.",
    ]
    return "\n".join(lines)


now = int(time.time())

if not predictions:
    if todo is None:
        print("prediction-resolve-sweep: nothing overdue, no todo to close")
        sys.exit(0)
    # Close it, never delete it. The row is the anchor for its nag rule and its
    # revision history; deleting it means the next overdue prediction starts a
    # fresh todo with no record of what came before.
    call("PATCH", "%s/api/items/%s" % (noteboard, todo["id"]), {"status": "done"})
    print("prediction-resolve-sweep: nothing overdue — closed todo %s" % todo["id"])
    sys.exit(0)

body = build_body(predictions, now)

if todo is None:
    tzid = os.environ.get("SWEEP_TZID", "America/Los_Angeles")
    anchor = datetime.datetime.now(ZoneInfo(tzid)).replace(
        hour=9, minute=0, second=0, microsecond=0
    )
    # The nag rule is written once, here, and never rewritten below. noteboard
    # does not snapshot `schedule` in item_revisions, so a PATCH that resent it
    # would destroy a cadence the user had tuned, with no revision to restore.
    created = call(
        "POST",
        "%s/api/items" % noteboard,
        {
            "type": "todo",
            "title": title,
            "body": body,
            "tags": ["prediction-store", "calibration"],
            "status": "open",
            "priority": 2,
            "schedule": {
                # Anchor the rule in the rule's own zone, not the host's. This box
                # runs on UTC, and an anchor stamped with a UTC offset under a
                # tzid of America/Los_Angeles reads as 2am local.
                "dtstart": anchor.isoformat(),
                "tzid": tzid,
                "remind": {
                    "nag": "FREQ=WEEKLY;BYDAY=MO;BYHOUR=9;BYMINUTE=0;BYSECOND=0",
                    "channels": ["digest"],
                },
            },
        },
    )
    print(
        "prediction-resolve-sweep: created todo %s listing %d overdue prediction(s)"
        % (created["id"], len(predictions))
    )
    sys.exit(0)

call(
    "PATCH",
    "%s/api/items/%s" % (noteboard, todo["id"]),
    {"body": body, "status": "open"},
)
print(
    "prediction-resolve-sweep: updated todo %s with %d overdue prediction(s)"
    % (todo["id"], len(predictions))
)
PYTHON
