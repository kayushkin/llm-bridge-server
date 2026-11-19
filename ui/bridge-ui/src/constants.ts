export const TRANSPORT_LABEL: Record<string, string> = {
  local: 'Local',
  ssh: 'SSH',
  runner: 'Runner',
}

// Note: harness label/emoji/image/tint live on the server-side HarnessInfo
// (delivered via /harnesses). UIs read those fields directly — no client-side
// fallback maps. If a harness shows up unlabeled/uncolored, the fix is to
// register it server-side, not patch around it here.

/** localStorage key for the board the kanban page last opened. The board page
 *  writes it on every selection; the board settings page reads it when the URL
 *  names no board, so "⚙ Board settings" and a bare `/kanban/settings` land on
 *  the same board. */
export const KANBAN_LAST_BOARD_STORAGE_KEY = 'bk:lastBoardId'
