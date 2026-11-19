import type { SessionUIState } from '../../types'

export type StatusDotState = SessionUIState | (string & {})

// model_generating renders three staggered dots (typing-indicator
// wave). Every other state renders the dot itself, with state-specific
// glyphs and animations attached via CSS. Keeps presentation in styles.css
// so adding a new state means a CSS rule, not a React change.
export function StatusDot({ state, title, className }: {
  state: StatusDotState
  title?: string
  className?: string
}) {
  return (
    <span
      className={`bc-status-dot bc-status-dot-${state}${className ? ` ${className}` : ''}`}
      title={title}
      aria-label={title}
    >
      {state === 'model_generating' ? (
        <>
          <span className="bc-status-dot-blip" />
          <span className="bc-status-dot-blip" />
          <span className="bc-status-dot-blip" />
        </>
      ) : null}
    </span>
  )
}
