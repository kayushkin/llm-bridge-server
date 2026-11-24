#!/usr/bin/env bash
# Backfill message_bodies by fetching every indexed message that has no
# recorded payload yet, through the ordinary GET /api/messages/{id} route —
# whose side effect (recordFullMessage) is what stores the body. No special
# backfill code path exists on the server, so what this warms is exactly what
# live traffic warms.
#
# Every fetch is a live provider round trip (Gmail API, IMAP, Graph), so this
# is deliberately slow: one message at a time with a pause between. Safe to
# interrupt and re-run — already-stored bodies are skipped by the query.
#
# Usage: MAILSTACK_TOKEN=... ./backfill-message-bodies.sh [limit]
set -euo pipefail

MAILSTACK_URL="${MAILSTACK_URL:-http://localhost:8195}"
DB="${MAILSTACK_DB:-$HOME/.config/mailstack/accounts.db}"
LIMIT="${1:-100000}"
PAUSE="${PAUSE:-0.4}"

if [ -z "${MAILSTACK_TOKEN:-}" ]; then
    echo "MAILSTACK_TOKEN is required" >&2
    exit 2
fi

total=0 ok=0 failed=0
while IFS='|' read -r account provider; do
    [ -z "$provider" ] && continue
    total=$((total + 1))
    if curl -sfS -o /dev/null \
        -H "Authorization: Bearer $MAILSTACK_TOKEN" \
        "$MAILSTACK_URL/api/messages/$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=''))" "$provider")?account=$account"; then
        ok=$((ok + 1))
    else
        failed=$((failed + 1))
        echo "FAILED: $account:$provider" >&2
    fi
    if [ $((total % 25)) -eq 0 ]; then
        echo "progress: $total fetched ($ok ok, $failed failed)"
    fi
    sleep "$PAUSE"
done < <(sqlite3 "$DB" "
    SELECT m.account_id, m.provider_id
    FROM messages m
    LEFT JOIN message_bodies b
      ON b.account_id = m.account_id
     AND b.folder_scope = m.folder_scope
     AND b.provider_id = m.provider_id
    WHERE b.id IS NULL
      -- Inbox rows only. Two reasons, either sufficient: the account-scoped
      -- GET cannot reach an IMAP UID outside the selected inbox (measured
      -- 2026-08-27: all 131 Sent/Spam/Trash/Drafts rows 404 forever), and
      -- spam/sent mail does not belong in a similarity corpus that feeds the
      -- email-classifier worked examples. folder_scope is '' for providers
      -- with account-wide ids (Gmail, Graph, local) and the mailbox name for
      -- IMAP, where only INBOX is wanted.
      AND m.folder_scope IN ('', 'INBOX')
    ORDER BY m.date_ms DESC
    LIMIT $LIMIT
" -separator '|')

echo "done: $total fetched, $ok stored, $failed failed"
sqlite3 "$DB" "SELECT 'bodies now stored: ' || COUNT(*) FROM message_bodies"
