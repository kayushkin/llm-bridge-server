import { PANE_KEYS, type PaneDrawable, type PaneKey } from './panePersistence'

/** What each pane is called on the switch. A record rather than a label baked into the
 *  loop, so adding a pane to `PANE_KEYS` is a type error here until it is named. */
const PANE_LABELS: Record<PaneKey, string> = {
  turns: 'Turns',
  timeline: 'Timeline',
  kanban: 'Cards',
  git: 'Git',
  attach: 'Terminal',
}

interface MinimalPaneSwitchProps {
  /** The pane on screen. */
  pane: PaneKey
  /** Show this pane instead. */
  onPick: (pane: PaneKey) => void
  /** Which conditionally-drawable panes this session can draw. */
  drawable: PaneDrawable
}

/** The mobile pane switch: pick ONE of the thread's panes.
 *
 *  ## Why this is chat's own component and not bridge-ui's
 *
 *  bridge-ui ships a `MinimalPaneSwitch` and it is the one piece of the minimal chrome
 *  that could not be mounted here unmodified — it hardcodes five of its own `PaneKey`s
 *  (`thread`, `git` and `kanban` among them), and this page can draw none of those. Its
 *  three siblings — `MinimalTopBar`, `SessionDrawer`, `ChromeSheet` — take no pane
 *  vocabulary at all and ARE imported from the library rather than copied.
 *
 *  It ships no CSS: `bc-mc-paneswitch` and `bc-mc-paneswitch-btn` are bridge-ui's own
 *  classes, already in the stylesheet dash loads.
 *
 *  ## Why a one-of-N switch, when the desktop header's pane row is not one
 *
 *  The desktop toggles are independent flags — Turns and Timeline can both be up, which
 *  is the whole point of that row. One pane wide, "both" is not an available answer, so
 *  the mobile control asks the question it can actually act on and the two records stay
 *  separate. `panePersistence.ts` records why neither reading is derived from the other.
 *
 *  Raw is deliberately not a button here. It is a MODE of the Turns pane rather than a
 *  pane (see `panePersistence.ts`), so it would not be one of these N; it is the lens
 *  chip in that pane's corner, on a phone as on a desktop.
 *
 *  Terminal is offered only when the session can draw it (`drawable`): offered on an
 *  events session it silently showed Turns instead. */
export default function MinimalPaneSwitch({ pane, onPick, drawable }: MinimalPaneSwitchProps) {
  return (
    <div className="bc-mc-paneswitch" role="tablist" aria-label="Pane">
      {PANE_KEYS.filter(key => key !== 'attach' || drawable.attach === true).map(key => (
        <button
          key={key}
          type="button"
          role="tab"
          aria-selected={pane === key}
          className={`bc-mc-paneswitch-btn ${pane === key ? 'bc-mc-paneswitch-btn-active' : ''}`}
          onClick={() => onPick(key)}
        >
          {PANE_LABELS[key]}
        </button>
      ))}
    </div>
  )
}
