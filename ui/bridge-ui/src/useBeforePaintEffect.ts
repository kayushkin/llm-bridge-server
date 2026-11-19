import { useEffect, useLayoutEffect } from 'react'

/**
 * `useLayoutEffect` in the browser, `useEffect` on the server.
 *
 * Anything that reads or writes scroll position has to run before the browser
 * paints, or the user sees the content in the wrong place and then watches it
 * correct itself. On the server there is no layout to run before, and React
 * warns about the layout variant there — and `npm run check` and
 * `npm run pane-cost` both render these panes through react-dom/server, so
 * the warning would land in the instrument's output.
 */
export const useBeforePaintEffect = typeof window === 'undefined' ? useEffect : useLayoutEffect
