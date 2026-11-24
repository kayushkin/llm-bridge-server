#!/usr/bin/env python3
"""Create the boards the Northwind demo runs on.

This sets up EMPTY boards and nothing else: columns, the priority ladder, and
the working week. It used to create nine hand-written cards as well, and those
cards were the problem. They were written before the repository existed in its
current shape, and one of them asked for a Node build image to be bumped in a
Go service that has never contained a line of JavaScript. A real agent picked
it up, checked every branch and all twelve commits, refused, and wrote down
why — behaving exactly as instructed, at the cost of a real session.

Cards now arrive the way they will in use: `demo-mail-generator` writes work
mail about the actual repository, and `email-classifier`, reading the Work board's `classifier` setting (vocabulary `work`), files
that mail onto the board. Nothing here invents work.

Two boards, because they answer different questions about the same cards:

  Work board      the EMAIL view — what arrived and whether it needs a person.
                  Its columns are the ones email-classifier looks up by name.
  Northwind work  the AGENT view — queued, running, done. Its columns are the
                  ones kanban-curator requires by name, so closing a card is
                  done by the tool that already does it.

A card sits on both. Same noteboard item, two placements, so closing it in one
closes it in the other.

    ./scripts/seed_work_board.py           # create whatever is missing
    ./scripts/seed_work_board.py --purge   # hard-delete the cards and the boards
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request

DEFAULT_KANBAN_URL = os.environ.get("KANBAN_STORE_URL", "http://localhost:8305")

BUSINESS_HOURS = {
    "tzid": "America/Los_Angeles",
    "days": ["MO", "TU", "WE", "TH", "FR"],
    "start": "09:00",
    "end": "17:00",
}

# P0 is the top rung and the top rung is the HIGHEST priority_value — noteboard
# sorts priority descending, and every list view in the stack depends on that.
PRIORITY_LEVELS = [
    {"priority_value": 5, "label": "P0", "budget_seconds": 2 * 3600},
    {"priority_value": 4, "label": "P1", "budget_seconds": 8 * 3600},
    {"priority_value": 3, "label": "P2", "budget_seconds": 24 * 3600},
    {"priority_value": 2, "label": "P3", "budget_seconds": 7 * 24 * 3600},
    {"priority_value": 1, "label": "P4", "budget_seconds": 30 * 24 * 3600},
]

BOARDS = [
    {
        "name": "Work board",
        "description": (
            "The email view of the Northwind team's work. Cards are filed here by "
            "email-classifier running with the work vocabulary; the mail behind them "
            "lives in the demo-work mailstack account. Column names are the ones the "
            "classifier looks up."
        ),
        # budget_clock_state is what landing in a column means for the card's clock.
        "columns": [
            {"name": "No action", "position": 1.0, "color": "#6b7280", "budget_clock_state": "stopped"},
            {"name": "Action needed", "position": 2.0, "color": "#d97706", "budget_clock_state": "running"},
            {"name": "Waiting on reply", "position": 2.5, "color": "#7c3aed", "budget_clock_state": "paused"},
            {"name": "Action completed", "position": 3.0, "color": "#059669",
             "auto_status": "done", "budget_clock_state": "stopped"},
        ],
    },
    {
        "name": "Northwind work",
        "description": (
            "The agent view of the same cards. demo-worker promotes a card here and "
            "dispatches an agent for it; kanban-curator closes it. Column names are "
            "the ones the curator requires, so closing is done by the tool that "
            "already does it rather than a second one."
        ),
        "columns": [
            {"name": "Queued", "position": 1.0, "color": "#6b7280", "budget_clock_state": "running"},
            {"name": "In Progress", "position": 2.0, "color": "#d97706", "budget_clock_state": "running"},
            {"name": "Blocked", "position": 3.0, "color": "#7c3aed", "budget_clock_state": "paused"},
            {"name": "Done", "position": 4.0, "color": "#059669",
             "auto_status": "done", "budget_clock_state": "stopped"},
            {"name": "Failed", "position": 5.0, "color": "#dc2626",
             "auto_status": "archived", "budget_clock_state": "stopped"},
        ],
    },
]


class ApiError(RuntimeError):
    pass


def request(base_url: str, method: str, path: str, body: dict | None = None):
    """One HTTP call to kanban-store. Any non-2xx raises — a setup that half
    succeeded and said nothing would leave boards nobody can trust."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base_url.rstrip("/") + path,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"} if data else {},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            raw = response.read()
    except urllib.error.HTTPError as exc:
        raise ApiError(f"{method} {path} → {exc.code}: {exc.read().decode(errors='replace')}") from exc
    except urllib.error.URLError as exc:
        raise ApiError(f"{method} {path} → {exc.reason} (is kanban-store running on {base_url}?)") from exc
    return json.loads(raw) if raw else None


def find_board(base_url: str, name: str) -> dict | None:
    for board in request(base_url, "GET", "/api/boards?include_archived=true") or []:
        if board["name"] == name:
            return board
    return None


def setup(base_url: str) -> None:
    """Create whatever is missing, and leave whatever is already right alone."""
    for spec in BOARDS:
        board = find_board(base_url, spec["name"])
        if board is None:
            board = request(base_url, "POST", "/api/boards",
                            {"name": spec["name"], "description": spec["description"]})
            print(f'created board {spec["name"]!r} → {board["id"]}')
        else:
            print(f'board {spec["name"]!r} exists → {board["id"]}')

        request(base_url, "PATCH", f"/api/boards/{board['id']}", {"business_hours": BUSINESS_HOURS})
        request(base_url, "PUT", f"/api/boards/{board['id']}/priority-levels", {"levels": PRIORITY_LEVELS})

        # A missing column is created; an existing one is left as it is, because
        # a column's clock classification is a decision somebody may have changed
        # deliberately and this script is not the place to overrule it.
        existing = {c["name"] for c in (request(base_url, "GET", f"/api/boards/{board['id']}/columns") or [])}
        for column in spec["columns"]:
            if column["name"] in existing:
                continue
            request(base_url, "POST", f"/api/boards/{board['id']}/columns", column)
            print(f'  + column {column["name"]}')


def purge(base_url: str) -> None:
    """Hard-delete every card on the demo boards, then the boards.

    Destructive on purpose: ?hard=true purges the underlying noteboard items
    rather than leaving orphan todos behind. Links go first — a hard card delete
    drops the placements but leaves the links, still readable, pointing at a
    card that no longer exists.
    """
    for spec in BOARDS:
        board = find_board(base_url, spec["name"])
        if board is None:
            print(f'no board named {spec["name"]!r}')
            continue
        view = request(base_url, "GET", f"/api/boards/{board['id']}/cards")
        card_ids = [card["placement"]["card_id"]
                    for column in (view.get("columns") or [])
                    for card in (column.get("cards") or [])]
        card_ids += [card["placement"]["card_id"] for card in (view.get("orphans") or [])]

        links_removed = 0
        for card_id in card_ids:
            for link in request(base_url, "GET", f"/api/cards/{card_id}/links") or []:
                request(base_url, "DELETE", f"/api/links/{link['id']}")
                links_removed += 1
            # A card on both boards is one item; deleting it from the first board
            # takes it off the second too, so a second delete is a 404 we ignore.
            try:
                request(base_url, "DELETE", f"/api/cards/{card_id}?hard=true")
            except ApiError:
                pass
        request(base_url, "DELETE", f"/api/boards/{board['id']}")
        print(f'purged {len(card_ids)} cards, {links_removed} links, and board {spec["name"]!r}')


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--kanban-url", default=DEFAULT_KANBAN_URL,
                        help=f"kanban-store base URL (default {DEFAULT_KANBAN_URL})")
    parser.add_argument("--purge", action="store_true",
                        help="hard-delete the cards and the boards, then exit")
    args = parser.parse_args()

    try:
        if args.purge:
            purge(args.kanban_url)
        else:
            setup(args.kanban_url)
    except ApiError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
