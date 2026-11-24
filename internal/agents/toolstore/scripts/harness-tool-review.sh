#!/usr/bin/env bash
# Daily report of the harness tools that need a person to look at them.
#
# Run by a scheduler shell job. It reads three things:
#   1. tool-store harness rows tagged "unreviewed": tools a harness reported
#      that nobody had registered, created switched off
#      (POST /harness-tools/observed);
#   2. claude_code harness rows no session has reported for 14 days
#      (GET /tools?kind=harness&not_seen_since=…). Only claude_code reports its
#      tool list, so codex rows are not checked this way. A row never reported
#      at all is listed only once reports have been arriving for 14 days (the
#      workspace below records when this job first saw one), so the days after
#      reporting starts do not list every tool;
#   3. `codex features list`, compared with the previous run's output, which
#      this script keeps in one noteboard workspace tagged
#      harness-tool-review-state. It never creates tool-store rows for codex
#      features.
# It keeps one open noteboard todo tagged harness-tool-review: it rewrites the
# body of the open one, or creates it, and leaves it alone when there is
# nothing to report.
#
# Environment (required; no defaults, so a missing one stops the run):
#   TOOL_STORE_URL  e.g. http://localhost:8302
#   NOTEBOARD_URL   e.g. http://localhost:8191
# `codex` must be on PATH. Any failure to reach tool-store, noteboard or codex
# exits non-zero, so the scheduler records the run as an error.
set -euo pipefail

: "${TOOL_STORE_URL:?TOOL_STORE_URL must be set, e.g. http://localhost:8302}"
: "${NOTEBOARD_URL:?NOTEBOARD_URL must be set, e.g. http://localhost:8191}"
command -v codex >/dev/null || { echo "codex is not on PATH ($PATH)" >&2; exit 1; }

export TOOL_STORE_URL NOTEBOARD_URL
exec python3 - <<'PYTHON'
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

TOOL_STORE_URL = os.environ["TOOL_STORE_URL"].rstrip("/")
NOTEBOARD_URL = os.environ["NOTEBOARD_URL"].rstrip("/")
TODO_TAG = "harness-tool-review"
STATE_TAG = "harness-tool-review-state"
NOT_SEEN_DAYS = 14
STATE_BEGIN = "<!-- codex-features-list-begin -->"
STATE_END = "<!-- codex-features-list-end -->"
REPORTS_FIRST_SEEN = re.compile(r"<!-- claude-code-reports-first-seen: (\d+) -->")


def call(method, url, body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.loads(response.read() or b"null")
    except urllib.error.HTTPError as error:
        sys.exit(f"{method} {url}: HTTP {error.code}: {error.read().decode(errors='replace')}")
    except (urllib.error.URLError, OSError) as error:
        sys.exit(f"{method} {url}: {error}")


def format_time(unix_seconds):
    if not unix_seconds:
        return "never"
    return time.strftime("%Y-%m-%d %H:%M UTC", time.gmtime(unix_seconds))


# --- tool-store -------------------------------------------------------------

unreviewed = call("GET", f"{TOOL_STORE_URL}/tools?kind=harness&tag=unreviewed")
not_seen_since = int(time.time()) - NOT_SEEN_DAYS * 86400
not_seen = call("GET", f"{TOOL_STORE_URL}/tools?kind=harness&harness=claude_code&not_seen_since={not_seen_since}")
# A tool that is both unreviewed and not seen is listed once, as unreviewed.
unreviewed_ids = {tool["id"] for tool in unreviewed}
not_seen = [tool for tool in not_seen if tool["id"] not in unreviewed_ids]
claude_code_tools = call("GET", f"{TOOL_STORE_URL}/tools?kind=harness&harness=claude_code")

# --- codex ------------------------------------------------------------------

codex = subprocess.run(["codex", "features", "list"], capture_output=True, text=True, timeout=120)
if codex.returncode != 0:
    sys.exit(f"codex features list exited {codex.returncode}: {codex.stderr.strip()}")
codex_output = codex.stdout.strip()
if not codex_output:
    sys.exit("codex features list printed nothing")

FEATURE_LINE = re.compile(r"^(\S+)\s+(.+?)\s+(true|false)$")


def parse_features(text):
    features = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        match = FEATURE_LINE.match(line.strip())
        if not match:
            sys.exit(f"cannot read this line of `codex features list`: {line!r}")
        name, stage, default = match.groups()
        features[name] = (stage, default)
    return features


current_features = parse_features(codex_output)

workspaces = call("GET", f"{NOTEBOARD_URL}/api/items?type=workspace&tag={STATE_TAG}&include_held=true&limit=10")
if len(workspaces) > 1:
    sys.exit(f"{len(workspaces)} workspaces carry tag {STATE_TAG}; want one: {[item['id'] for item in workspaces]}")
workspace = workspaces[0] if workspaces else None

# When this job first saw a claude_code report: kept in the workspace, else
# the oldest report on a claude_code row now, else none yet.
reports_first_seen = None
if workspace is not None:
    match = REPORTS_FIRST_SEEN.search(workspace.get("body", ""))
    if match:
        reports_first_seen = int(match.group(1))
if reports_first_seen is None:
    reported_times = [tool["last_seen_at"] for tool in claude_code_tools if tool.get("last_seen_at")]
    if reported_times:
        reports_first_seen = min(reported_times)
never_reported_held_back = []
if reports_first_seen is None or reports_first_seen > not_seen_since:
    never_reported_held_back = [tool for tool in not_seen if not tool.get("last_seen_at")]
    not_seen = [tool for tool in not_seen if tool.get("last_seen_at")]

codex_changes = []
codex_baseline_note = None
if workspace is None:
    codex_baseline_note = "no previous `codex features list` was kept; this run records the first one"
else:
    body = workspace.get("body", "")
    if STATE_BEGIN not in body or STATE_END not in body:
        sys.exit(f"workspace {workspace['id']} holds no codex features list between {STATE_BEGIN} and {STATE_END}")
    previous_text = body.split(STATE_BEGIN, 1)[1].split(STATE_END, 1)[0].strip().strip("`").strip()
    previous_features = parse_features(previous_text)
    for name in sorted(set(previous_features) | set(current_features)):
        before = previous_features.get(name)
        after = current_features.get(name)
        if before == after:
            continue
        if before is None:
            codex_changes.append(f"`{name}` appeared: stage {after[0]}, default {after[1]}")
        elif after is None:
            codex_changes.append(f"`{name}` disappeared (was stage {before[0]}, default {before[1]})")
        else:
            codex_changes.append(f"`{name}` changed: stage {before[0]} -> {after[0]}, default {before[1]} -> {after[1]}")

# --- report -----------------------------------------------------------------

def tool_url(tool):
    return f"{TOOL_STORE_URL}/tools/{tool['id']}"


sections = []
if unreviewed:
    lines = [f"## {len(unreviewed)} harness tool(s) a harness reported and nobody has reviewed",
             "",
             "Each is switched off everywhere until reviewed. Set its tags (this drops `unreviewed`), then enable it if it should be on.",
             ""]
    for tool in unreviewed:
        lines += [
            f"### `{tool['name']}` (tool id {tool['id']})",
            f"- {tool.get('description', '')}",
            f"- last reported: {format_time(tool.get('last_seen_at'))}; enabled: {str(tool['enabled']).lower()}",
            "```bash",
            f"curl -sf -X PATCH {tool_url(tool)} -H 'Content-Type: application/json' -d '{{\"tags\":[\"read-only\"]}}'",
            f"# or: curl -sf -X PATCH {tool_url(tool)} -H 'Content-Type: application/json' -d '{{\"tags\":[\"effects\",\"runs-commands\"]}}'",
            f"curl -sf -X POST {tool_url(tool)}/enable",
            "```",
            "",
        ]
    sections.append("\n".join(lines))
if not_seen:
    lines = [f"## {len(not_seen)} claude_code tool(s) no session has reported since {format_time(not_seen_since)}",
             "",
             "Claude Code may have dropped or renamed these, or may offer them only where nothing reports. `never` means no session has reported the tool since tool-store started recording reports. Only `-p` sessions report: a pty session reports no tool list (llm-bridge-claudecode `PTY-COVERAGE.md`), and `-p` hides tools an interactive session has — on Claude Code 2.1.283, `AskUserQuestion`, `EnterPlanMode` and `ExitPlanMode` appear only with `--permission-prompt-tool`. Switching a row off denies the tool in every session, pty ones included, so first read the `tools` list in the `init` event of `claude -p --output-format stream-json --verbose` and check whether pty sessions use it. Switch off one that is gone; a seeded one also needs removing from `cmd/tool-store/harness_seeds.go`.",
             ""]
    for tool in not_seen:
        lines.append(f"- `{tool['name']}` (tool id {tool['id']}), last reported {format_time(tool.get('last_seen_at'))}, enabled {str(tool['enabled']).lower()}: `curl -sf -X POST {tool_url(tool)}/disable`")
    sections.append("\n".join(lines + [""]))
if codex_changes:
    lines = [f"## {len(codex_changes)} change(s) in `codex features list` since the last run",
             "",
             "tool-store holds no rows for codex features; a change that adds or removes a tool needs `codexTools` in `cmd/tool-store/harness_seeds.go` edited by hand.",
             ""]
    lines += [f"- {change}" for change in codex_changes]
    sections.append("\n".join(lines + [""]))

run_time = time.strftime("%Y-%m-%d %H:%M UTC", time.gmtime())

if sections:
    body = (f"Written by scheduler job harness-tool-review (`scripts/harness-tool-review.sh` in tool-store) at {run_time}. "
            "It rewrites this body each day while there is something to report.\n\n" + "\n".join(sections))
    title = "Review harness tools: " + ", ".join(
        part for part in [
            f"{len(unreviewed)} unreviewed" if unreviewed else "",
            f"{len(not_seen)} not reported for {NOT_SEEN_DAYS} days" if not_seen else "",
            f"{len(codex_changes)} codex feature changes" if codex_changes else "",
        ] if part)
    todos = call("GET", f"{NOTEBOARD_URL}/api/items?type=todo&status=open&tag={TODO_TAG}&include_held=true&limit=10")
    if len(todos) > 1:
        sys.exit(f"{len(todos)} open todos carry tag {TODO_TAG}; want one: {[item['id'] for item in todos]}")
    if todos:
        todo = todos[0]
        if todo.get("held_at"):
            print(f"todo {todo['id']} is held ({todo.get('hold_reason', '')}); leaving it alone")
        else:
            call("PATCH", f"{NOTEBOARD_URL}/api/items/{todo['id']}", {"title": title, "body": body})
            print(f"updated todo {todo['id']}")
    else:
        todo = call("POST", f"{NOTEBOARD_URL}/api/items", {"type": "todo", "title": title, "body": body, "tags": [TODO_TAG]})
        print(f"created todo {todo['id']}")
    print(body)
else:
    print("nothing to report")
if never_reported_held_back:
    print(f"{len(never_reported_held_back)} claude_code tools have never been reported; not listed, because reports have not been "
          f"arriving for {NOT_SEEN_DAYS} days (first seen: {format_time(reports_first_seen)})")

# The codex state is written last, so a run that failed to write the todo
# reports the same changes again tomorrow rather than losing them.
state_body = (f"Working memory for scheduler job harness-tool-review (`scripts/harness-tool-review.sh` in tool-store): "
              f"the `codex features list` output of the last run, at {run_time}, which the next run compares against. "
              "Not work to do.\n\n"
              + (f"<!-- claude-code-reports-first-seen: {reports_first_seen} -->\n"
                 f"This job first saw a claude_code tool report at {format_time(reports_first_seen)}.\n\n"
                 if reports_first_seen is not None else "")
              + STATE_BEGIN + "\n```\n" + codex_output + "\n```\n" + STATE_END + "\n")
if workspace is None:
    workspace = call("POST", f"{NOTEBOARD_URL}/api/items", {
        "type": "workspace", "title": "Workspace — harness-tool-review", "body": state_body, "tags": [STATE_TAG]})
    print(f"created workspace {workspace['id']} ({codex_baseline_note})")
else:
    call("PATCH", f"{NOTEBOARD_URL}/api/items/{workspace['id']}", {"body": state_body})
    print(f"updated workspace {workspace['id']}")
PYTHON
