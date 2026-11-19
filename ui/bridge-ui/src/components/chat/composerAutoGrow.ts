/** The height the auto-growing composer must be given so its content fits.
 *
 *  `scrollHeight` is content plus padding and **excludes the border**. dash, which
 *  mounts this component, resets `* { box-sizing: border-box }`
 *  (`src/index.css:55`), which makes an assigned height
 *  the height of the *box* — border included. Assigning a bare `scrollHeight`
 *  therefore lands a border-width short of the content it was measured from, so
 *  the content never fits and `overflow-y: auto` gives the textarea a scrollbar at
 *  **every** size rather than only past the cap. On this origin that was 2px, and
 *  it was small enough to read as a rendering artifact for as long as it shipped.
 *
 *  The border is added back only under `border-box`. Under `content-box` the
 *  assigned height already excludes the border, so adding it would overshoot by
 *  the same amount in the other direction. Reading the used value rather than
 *  assuming either one keeps this correct for a host that resets neither.
 *
 *  No cap is applied here. `.bc-composer-input` carries `max-height: 220px` in
 *  this package's own `styles.css` (both the base rule and the themed one), so the
 *  browser clamps whatever inline height we set and `overflow-y: auto` takes over
 *  past it. This function used to compare against a duplicate `MAX_INPUT_PX = 220`
 *  in this file, which had to be kept in step with the stylesheet by hand — and
 *  which was wrong by the border anyway, since it was compared against a
 *  `scrollHeight` that means something slightly different from the height it set.
 *  Letting the stylesheet own the number leaves it in one place. */
export function composerAutoGrowHeightPx(measurements: {
  scrollHeight: number
  boxSizing: string
  borderTopWidth: string
  borderBottomWidth: string
}): number {
  const { scrollHeight, boxSizing, borderTopWidth, borderBottomWidth } = measurements
  if (boxSizing !== 'border-box') return scrollHeight
  const top = parseFloat(borderTopWidth) || 0
  const bottom = parseFloat(borderBottomWidth) || 0
  return scrollHeight + top + bottom
}
