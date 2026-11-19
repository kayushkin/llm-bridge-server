// Which of the thread's panes are on screen. chat used to draw exactly one — an
// exclusive `'turns' | 'raw' | 'timeline'` switch in `SessionHeader` — so Turns and
// Timeline could never be read at the same time. This record replaces the exclusive
// part of that switch with a per-pane visibility flag, which is what bridge-ui's
// `PanesHidden` (bridge-ui `chat/types.ts`) has always been.
//
// ⚠️ The key is chat's own, for the reason `sidebarPersistence.ts` states at length:
// dash served bridge-ui's chat beside its own while both existed — one origin and one
// localStorage. bridge-ui persists its own visibility inside the single
// `bridge-ui-workspaces` blob, keyed per workspace and validated by its own parser, so
// there is no shared row to join on even if the two pages wanted one.

import { chatKey, migrateLegacyKeys } from './storageKeys'

/** The panes chat can draw.
 *
 *  `raw` is not here, and that is the one call this record had to make. bridge-ui has no
 *  `PaneKey` for it, so the two vocabularies are not a superset relation and the answer
 *  could not be copied. It was measured instead: `TurnList` takes `view: 'turns' | 'raw'`
 *  (`TurnList.tsx`) and renders the same transcript either way — Raw keeps the
 *  duplicates, the source and the eventId that the collapsed view drops. One component,
 *  two renderings of one thing. `Timeline` is a different component over a different
 *  derivation (`selectTimeline`).
 *
 *  So Raw is a MODE of the Turns pane, alongside the MD/TXT toggle that already modifies
 *  it, and not a pane of its own. Making it a third pane would have offered the user
 *  "show Turns beside Raw", which is the same transcript twice.
 *
 *  `kanban` IS here, and it is a pane rather than a mode for the mirror-image reason:
 *  `LinkedKanbanPanel` is a different component over a different backend
 *  (kanban-store, polled — it has no notifier), showing the cards linked to the
 *  session rather than any rendering of the transcript. "Show Turns beside the cards"
 *  is a thing a reader wants; "show Turns beside Turns" is not.
 *
 *  `git` IS here for the same reason as `kanban`, and it is the one pane bridge-ui had
 *  that dash could not reach any other way: the orchestrator has its own page at
 *  `/orchestrator` and the kanban board has `/kanban`, but `GitPanel` was mounted from
 *  the chat workspace and nowhere else, so removing that chat would have removed the
 *  repo, the branch and the working-tree diff from dash entirely. It shows what the
 *  agent has DONE to the tree while the transcript shows what it said about it, which
 *  is the pairing the pane exists for.
 *
 *  `attach` is the terminal of a session running in pty mode, and it is the ONE pane
 *  whose visibility is not this record's decision alone. A session in events mode has no
 *  pty hub to join, so the pane cannot be drawn for it whatever the stored flag says —
 *  see `visiblePanes`, which takes that as an argument rather than reading it here. The
 *  flag still means what it means: it is the user's answer for the sessions where the
 *  question can be asked at all. */
export type PaneKey = 'turns' | 'timeline' | 'kanban' | 'git' | 'attach'

/** Draw order, left to right. The array is the layout — `visiblePanes` filters it and
 *  the row renders what comes back, so moving an entry here moves the pane on screen. */
export const PANE_KEYS: readonly PaneKey[] = ['turns', 'timeline', 'kanban', 'git', 'attach']

/** Hidden, not visible — bridge-ui's polarity, kept because an absent flag then means
 *  "shown", which is the state a pane should degrade to. */
export type PanesHidden = Record<PaneKey, boolean>

/** What chat has always opened as: the Turns pane, alone. Timeline starts hidden so
 *  the change is additive — nobody who never touches the toggles sees a different page. */
export const DEFAULT_PANES_HIDDEN: PanesHidden = {
  turns: false,
  timeline: true,
  kanban: true,
  git: true,
  attach: true,
}

// Keys live under the chat prefix. They were written under the page's old prefix while
// it had its old name, and `storageKeys.ts` carries those rows over on first read — see
// the `migrateLegacyKeys` call in `storage()` below. Add a key here AND to MIGRATED_KEYS.
const PANES_HIDDEN_KEY = chatKey('panes-hidden')

/** How the visible panes divide the row between them, in flex-grow units.
 *
 *  Grow units and not pixels, and not percentages. The panes are laid out
 *  `flex: <grow> 1 0`, so a pane's width is its share of the pair's total — which
 *  means a stored record stays meaningful at a window width it was never written at.
 *  A pixel record would not: restore 900px of Turns into a 700px window and the
 *  arithmetic is already wrong before the first paint.
 *
 *  ⚠️ Only the RATIO is meaningful, never the magnitudes. `{turns: 2, timeline: 1}`
 *  and `{turns: 20, timeline: 10}` are the same layout, and `splitGrowUnitsAfterDrag`
 *  conserves the pair's total rather than normalizing it — so whatever total a drag
 *  starts from is the total it writes back. Nothing may read a single pane's number
 *  and conclude anything from it alone. */
export type PaneSizes = Record<PaneKey, number>

/** Even. Both panes `flex: 1 1 0` is what `styles.css` already gives them, so a user
 *  who never drags the boundary sees exactly the page they saw before sizes existed. */
export const DEFAULT_PANE_SIZES: PaneSizes = {
  turns: 1,
  timeline: 1,
  kanban: 1,
  git: 1,
  attach: 1,
}

const PANE_SIZES_KEY = chatKey('pane-sizes')

/** Which single pane the minimal (mobile) chrome draws.
 *
 *  Below 640px the thread is one pane wide, so the visibility RECORD above cannot
 *  answer what to show: it can say "both", and both is what does not fit. The minimal
 *  chrome asks a different question — which ONE — so it gets its own answer rather
 *  than a reading of `panesHidden` that would have to invent a winner when the user
 *  has revealed two panes on the desktop.
 *
 *  The two records are kept apart on purpose, in both directions: widening the phone
 *  back out restores exactly the panes the user had arranged there, and picking
 *  Timeline on a phone does not hide Turns on the desktop.
 *
 *  ⚠️ This is chat's own key, and NOT bridge-ui's `bridge-mobile-pane`. The library
 *  stores the same idea under that key typed as its own `PaneKey` — seven members,
 *  including `thread`, `git` and `kanban`, which this page cannot draw. dash serves
 *  both surfaces from one origin and one localStorage, so sharing the row would mean
 *  reading back a pane with no renderer here, and writing `turns` over a `git` the
 *  user chose on the chat at `/`. The vocabularies are not a superset relation
 *  (`PaneKey` above says why `raw` is not in this one either), so there is no row to
 *  join on — the same reasoning `sidebarPersistence.ts` and the header of this file
 *  already record. */
const MOBILE_PANE_KEY = chatKey('mobile-pane')

/** Turns, matching what a cold desktop load opens as. */
export const DEFAULT_MOBILE_PANE: PaneKey = 'turns'

/** The panes on screen, in draw order.
 *
 *  Derived from `PANE_KEYS` rather than from the record's own key order, because a
 *  record read back from storage carries whatever order it was written in — and with
 *  the merge in `loadPanesHidden` that is the order of whichever build wrote it last.
 *  Layout must not depend on that.
 *
 *  The row draws a drag handle between each ADJACENT pair of what this returns, so a
 *  pane hidden in the middle closes the gap and the two survivors become neighbors
 *  with one boundary — rather than leaving a handle that measures a pane nobody can
 *  see. */
export function visiblePanes(panesHidden: PanesHidden, drawable: PaneDrawable = {}): PaneKey[] {
  return PANE_KEYS.filter(key => {
    if (panesHidden[key]) return false
    if (key === 'attach') return drawable.attach === true
    return true
  })
}

/** What the visible panes' flex-grow numbers add up to, whatever the stored sizes are. */
export const VISIBLE_PANES_GROW_TOTAL = 100

/** The flex-grow each visible pane is drawn with: its stored size as a share of the
 *  visible panes' total, scaled to `VISIBLE_PANES_GROW_TOTAL`.
 *
 *  The stored sizes cannot be handed to CSS as they are. Flex items whose grow numbers
 *  add up to LESS THAN 1 take only that fraction of the free space and leave the rest of
 *  the row empty. A drag conserves its pair's total, so dragging Turns narrow against
 *  Timeline writes something like `{turns: 0.4, timeline: 1.6}`; hide Timeline — or open
 *  the page on a phone, which draws one pane — and Turns alone held a grow of 0.4, drew
 *  at its 240px floor and left an empty block beside it. Only the ratio between stored
 *  sizes means anything, so scaling the visible ones to a fixed total changes no layout
 *  that was already filling the row.
 *
 *  A pane that is not visible gets no entry. The drag handle keeps reading the STORED
 *  sizes, not these: it works in the pair's own total and writes that total back. */
export function growSharesOfVisiblePanes(
  paneSizes: PaneSizes,
  panes: readonly PaneKey[],
): Partial<Record<PaneKey, number>> {
  const total = panes.reduce((sum, key) => sum + paneSizes[key], 0)
  const shares: Partial<Record<PaneKey, number>> = {}
  for (const key of panes) shares[key] = (paneSizes[key] / total) * VISIBLE_PANES_GROW_TOTAL
  return shares
}

/** Which conditionally-drawable panes can be drawn for THIS session right now.
 *
 *  Only `attach` so far. It is separated from `PanesHidden` rather than folded into it
 *  because the two answer different questions and are owned by different things: hidden
 *  is the user's choice and persists, drawable is a fact about the session and changes
 *  under them. Writing "not drawable" into the stored record would forget the user's
 *  choice the first time they opened an events-mode session, and they would have to make
 *  it again on the next pty one.
 *
 *  Absent means NOT drawable, which is why the check above is `=== true`. A caller that
 *  has not yet learned the session's mode must not flash a terminal for a session that
 *  turns out to be in events mode — the pane would mount, ask for an attach token, and
 *  be told there is no hub. */
export interface PaneDrawable {
  attach?: boolean
}

/** Whether persistence works here at all. Checks the METHODS rather than the name — see
 *  chat-core's `webStorage.ts` for the Node/SSR/blocked-by-policy cases this covers.
 *  Duplicated from `sidebarPersistence.ts` rather than shared, for the reason
 *  `threadPersistence.ts` gives for its own copy: the three files are independent, and a
 *  shared helper would be the only thing tying their storage together. */
function storage(): Storage | null {
  try {
    if (typeof window === 'undefined') return null
    const candidate = window.localStorage
    if (
      !candidate ||
      typeof candidate.getItem !== 'function' ||
      typeof candidate.setItem !== 'function' ||
      typeof candidate.removeItem !== 'function'
    ) {
      return null
    }
    // First read after the rename carries the legacy rows across; see storageKeys.ts.
    migrateLegacyKeys(candidate, MIGRATED_KEYS)
    return candidate
  } catch {
    return null
  }
}

/** The stored visibility, merged over the defaults.
 *
 *  Merged rather than replaced, and per key rather than per record. A pane the stored
 *  record has never heard of — every pane added after the row was written — takes ITS
 *  OWN default, which is the only answer that stays right as `PaneKey` grows. Replacing
 *  the whole record on a partial read would instead give a new pane whatever `false`
 *  happens to mean for it.
 *
 *  Only booleans under known keys are honored. Anything else is treated as absent, so a
 *  truncated write or a hand-edited row degrades to the page chat has always opened as
 *  rather than to a pane set nobody chose.
 *
 *  ⚠️ There is no `Array.isArray` guard, and its absence is measured rather than
 *  overlooked. One was written and then deleted: removing it changed no answer, because
 *  an array IS an object and the merge below drops every key it does not know and every
 *  value that is not a boolean — an array's keys are `0`, `1`, `length`, never a
 *  `PaneKey`. A guard that cannot change an answer is one more thing claiming to do work
 *  it does not do. The object check stays, because it turns "the row held a number" into
 *  an answer rather than into a TypeError the catch below happens to absorb. */
export function loadPanesHidden(): PanesHidden {
  const store = storage()
  if (!store) return { ...DEFAULT_PANES_HIDDEN }
  try {
    const raw = store.getItem(PANES_HIDDEN_KEY)
    if (!raw) return { ...DEFAULT_PANES_HIDDEN }
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object') return { ...DEFAULT_PANES_HIDDEN }
    const record = parsed as Record<string, unknown>
    const merged = { ...DEFAULT_PANES_HIDDEN }
    for (const key of PANE_KEYS) {
      const value = record[key]
      if (typeof value === 'boolean') merged[key] = value
    }
    return merged
  } catch {
    return { ...DEFAULT_PANES_HIDDEN }
  }
}

/** Written whole and on both edges, never removed on the default. Hiding Turns and
 *  revealing it again are both choices, and the second one has to survive a reload as
 *  much as the first — removing the row instead would be indistinguishable from never
 *  having chosen, which matters the day a default changes. */
export function savePanesHidden(panesHidden: PanesHidden): void {
  const store = storage()
  if (!store) return
  try {
    store.setItem(PANES_HIDDEN_KEY, JSON.stringify(panesHidden))
  } catch {
    // A full or refused quota must not cost the user the click they just made.
  }
}

/** The stored sizes, merged over the defaults per key — `loadPanesHidden`'s rule, and
 *  for its reason: a pane added after the row was written takes its own default rather
 *  than whatever a missing key would coerce to.
 *
 *  ⚠️ A value must be finite AND strictly positive, and that is a correctness check
 *  rather than tidiness. `measureSplitDragGeometry` returns null on a pair whose grow
 *  units total zero or less, and a null geometry makes the handle a silent no-op — so a
 *  row holding `{"turns": 0, "timeline": 0}`, from a hand edit or a truncated write,
 *  would leave a resizer on screen that cannot be dragged and says nothing about why.
 *  A negative would be worse: flex treats it as invalid and the pair's total stops
 *  describing the pair. Rejecting per key means the good half of a half-corrupt row
 *  still survives. */
export function loadPaneSizes(): PaneSizes {
  const store = storage()
  if (!store) return { ...DEFAULT_PANE_SIZES }
  try {
    const raw = store.getItem(PANE_SIZES_KEY)
    if (!raw) return { ...DEFAULT_PANE_SIZES }
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object') return { ...DEFAULT_PANE_SIZES }
    const record = parsed as Record<string, unknown>
    const merged = { ...DEFAULT_PANE_SIZES }
    for (const key of PANE_KEYS) {
      const value = record[key]
      if (typeof value === 'number' && Number.isFinite(value) && value > 0) merged[key] = value
    }
    return merged
  } catch {
    return { ...DEFAULT_PANE_SIZES }
  }
}

/** Written whole, including a write back to the even split — for the reason
 *  `savePanesHidden` gives: dragging the boundary back to the middle, or
 *  double-clicking to reset it, is as much a choice as moving it was. */
export function savePaneSizes(paneSizes: PaneSizes): void {
  const store = storage()
  if (!store) return
  try {
    store.setItem(PANE_SIZES_KEY, JSON.stringify(paneSizes))
  } catch {
    // A full or refused quota must not cost the user the drag they just made.
  }
}

/** The stored mobile pane, or the default.
 *
 *  Checked against `PANE_KEYS` rather than merged, because this record holds one value
 *  and not a set: a stored name this page has no renderer for has no partial reading to
 *  salvage. A row left by an older build, or by a hand edit, degrades to Turns — the
 *  pane every load of this page has always opened with. */
export function loadMobilePane(): PaneKey {
  const store = storage()
  if (!store) return DEFAULT_MOBILE_PANE
  try {
    const raw = store.getItem(MOBILE_PANE_KEY)
    if (!raw) return DEFAULT_MOBILE_PANE
    if ((PANE_KEYS as readonly string[]).includes(raw)) return raw as PaneKey
    return DEFAULT_MOBILE_PANE
  } catch {
    return DEFAULT_MOBILE_PANE
  }
}

/** Whether the Turns pane's vibe legend is expanded.
 *
 *  Per PANE, not per turn and not per session: the legend explains a color scheme that
 *  is the same in every turn of every session, so a reader who has learnt it wants it
 *  shut everywhere and a reader who has not wants it open everywhere.
 *
 *  It lives here rather than in `threadPersistence.ts` for the same reason the two
 *  records above do — it is a property of what the pane is drawing, not of which session
 *  is open. */
const VIBE_LEGEND_KEY = chatKey('vibe-legend-open')
const MIGRATED_KEYS: readonly string[] = [PANES_HIDDEN_KEY, PANE_SIZES_KEY, MOBILE_PANE_KEY, VIBE_LEGEND_KEY]

/** Collapsed. The rails and glyphs are meant to be legible without it — the legend is
 *  for the first read, not for every one. */
export const DEFAULT_VIBE_LEGEND_OPEN = false

export function loadVibeLegendOpen(): boolean {
  const store = storage()
  if (!store) return DEFAULT_VIBE_LEGEND_OPEN
  try {
    const raw = store.getItem(VIBE_LEGEND_KEY)
    if (raw === null) return DEFAULT_VIBE_LEGEND_OPEN
    // Compared against the two values this writes, not coerced. A row holding anything
    // else was not written by this page, and truthiness would turn `"false"` into open.
    if (raw === 'true') return true
    if (raw === 'false') return false
    return DEFAULT_VIBE_LEGEND_OPEN
  } catch {
    return DEFAULT_VIBE_LEGEND_OPEN
  }
}

/** Written on both edges, including a close back to the default — for the reason
 *  `savePanesHidden` gives: shutting it again is a choice too. */
export function saveVibeLegendOpen(open: boolean): void {
  const store = storage()
  if (!store) return
  try {
    store.setItem(VIBE_LEGEND_KEY, open ? 'true' : 'false')
  } catch {
    // A full or refused quota must not cost the user the click they just made.
  }
}

/** Written on every pick, including a pick of the default — for the reason
 *  `savePanesHidden` states: choosing Turns back is as much a choice as leaving it. */
export function saveMobilePane(pane: PaneKey): void {
  const store = storage()
  if (!store) return
  try {
    store.setItem(MOBILE_PANE_KEY, pane)
  } catch {
    // A full or refused quota must not cost the user the click they just made.
  }
}
