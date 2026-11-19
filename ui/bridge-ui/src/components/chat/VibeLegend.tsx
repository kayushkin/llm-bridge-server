import { useCallback, useState } from 'react'
import { VIBES, VIBE_GLYPH, VIBE_LABEL, VIBE_MEANING } from './vibes'
import { loadVibeLegendOpen, saveVibeLegendOpen } from './panePersistence'
import styles from './Chat.module.css'

/** What the rails and glyphs beside an assistant's answer mean.
 *
 *  ONE per pane, not one per turn — the scheme is identical in every turn, so repeating
 *  it would be repeating the same six rows down the whole transcript.
 *
 *  It mounts as a pane-level sibling of the virtualized scroller, exactly like the
 *  "Compacting context…" strip and the jump-to-latest button, and for the same reason
 *  stated there: `bc-turns-body` is a virtua `VList` whose children are windowed rows, so
 *  anything placed inside it becomes row n+1, shifts the sticky-bottom index and vanishes
 *  when it scrolls out of the window.
 *
 *  It is deliberately NOT in the session header. chat's Turns pane has no header at all
 *  (todo `ff52df0e`), and inventing one to hold a legend would pre-empt a placement
 *  decision that is the user's — where chat's pane chrome lives is an open question with
 *  its own todo. A floating control needs no answer to it.
 *
 *  Its open state persists per pane (`panePersistence.ts`), read ONCE into state: this is
 *  chrome, and re-reading localStorage on every render of a streaming pane would be a
 *  synchronous storage hit per token. */
export default function VibeLegend() {
  const [open, setOpen] = useState<boolean>(() => loadVibeLegendOpen())

  const toggle = useCallback(() => {
    setOpen((wasOpen) => {
      const next = !wasOpen
      saveVibeLegendOpen(next)
      return next
    })
  }, [])

  return (
    <div className={styles.vibeLegend} data-open={open ? 'true' : 'false'}>
      <button
        type="button"
        className={styles.vibeLegendToggle}
        onClick={toggle}
        aria-expanded={open}
        title={open ? 'Hide the vibe legend' : 'What do the colored rails mean?'}
      >
        legend {open ? '▴' : '▾'}
      </button>
      {open && (
        <dl className={styles.vibeLegendBody}>
          {VIBES.map((vibe) => (
            <div key={vibe} className={styles.vibeLegendRow} data-vibe={vibe}>
              {/* The glyph is the same character the gutter draws, taken from the same
                  record — a legend that hard-coded its own copy could drift from the
                  thing it explains. */}
              <dt className={styles.vibeLegendTerm}>
                <span aria-hidden="true">{VIBE_GLYPH[vibe]}</span> {VIBE_LABEL[vibe]}
              </dt>
              <dd className={styles.vibeLegendMeaning}>{VIBE_MEANING[vibe]}</dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  )
}
