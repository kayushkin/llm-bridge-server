import { useState } from 'react'
import { capText, DOM_TEXT_LIMIT } from '../../toolPayloadPreview'

/** Tool text handed to the DOM at most DOM_TEXT_LIMIT characters at a time.
 *
 *  A 586 KB grep result written into one element is what froze the Raw view and the
 *  fallback tool card. Longer text shows its head and a button that reveals the rest;
 *  nothing is dropped, and the reader decides when the page pays for it. */
export default function CappedText({
  text,
  as: Tag = 'pre',
  className,
  style,
}: {
  text: string
  as?: 'pre' | 'span' | 'div'
  className?: string
  style?: React.CSSProperties
}) {
  const [showAll, setShowAll] = useState(false)
  const capped = capText(text)
  if (showAll || capped.hiddenCharacters === 0) {
    return (
      <Tag className={className} style={style}>
        {text}
      </Tag>
    )
  }
  return (
    <>
      <Tag className={className} style={style}>
        {capped.shown}
      </Tag>
      <button
        type="button"
        className="bc-tool-show-all"
        onClick={(e) => {
          // Inside a collapsible card whose click toggles it.
          e.stopPropagation()
          setShowAll(true)
        }}
      >
        Show the remaining {capped.hiddenCharacters.toLocaleString()} characters (first{' '}
        {DOM_TEXT_LIMIT.toLocaleString()} shown)
      </button>
    </>
  )
}
