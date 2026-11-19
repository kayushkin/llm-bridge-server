import { useCallback, useEffect, useRef } from 'react'
import { useMinimalChrome } from './MinimalChromeContext'

export function ChromeSheet() {
  const { sheetOpen, setSheetOpen, registerControlsSlot, setOverride } = useMinimalChrome()
  const slotRef = useRef<HTMLDivElement | null>(null)

  const setSlotNode = useCallback((el: HTMLDivElement | null) => {
    slotRef.current = el
    registerControlsSlot(el)
  }, [registerControlsSlot])

  useEffect(() => {
    if (!sheetOpen) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setSheetOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [sheetOpen, setSheetOpen])

  return (
    <>
      <div
        className={`bc-mc-scrim ${sheetOpen ? 'bc-mc-scrim-open' : ''}`}
        onClick={() => setSheetOpen(false)}
        aria-hidden={!sheetOpen}
      />
      <div
        className={`bc-mc-sheet ${sheetOpen ? 'bc-mc-sheet-open' : ''}`}
        role="dialog"
        aria-label="Chat controls"
        aria-hidden={!sheetOpen}
      >
        <div className="bc-mc-sheet-grabber" />
        <div className="bc-mc-sheet-header">
          <span className="bc-mc-sheet-title">Controls</span>
          <button
            type="button"
            className="bc-mc-close"
            onClick={() => setSheetOpen(false)}
            aria-label="Close"
          >×</button>
        </div>
        <div className="bc-mc-sheet-body">
          <div className="bc-mc-controls-slot" ref={setSlotNode} />
          <div className="bc-mc-sheet-footer">
            <button
              type="button"
              className="bc-mc-escape"
              onClick={() => { setOverride('full'); setSheetOpen(false) }}
            >Show full layout</button>
          </div>
        </div>
      </div>
    </>
  )
}
