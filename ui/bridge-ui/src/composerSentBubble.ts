// The message a send just took out of the composer, drawn as `.bc-composer-sent` in the
// composer's wrapper so a host theme can animate it leaving — dash's zine theme folds it
// into a speech bubble and flies it to its mascot. bridge-ui hides the element and does
// not animate it; it only says what was sent and where the text box stood.

export interface SentBubbleBox {
  left: number
  top: number
  width: number
  height: number
}

/** Where the text box stood inside the composer's wrapper, as CSS variables on the sent
 *  bubble: `--sent-left`, `--sent-top`, `--sent-width` and `--sent-height`, in px. A
 *  theme positions the bubble over the text box with them, and aims its flight from
 *  there. */
export function sentBubblePlacement(textBox: SentBubbleBox, wrapper: Pick<SentBubbleBox, 'left' | 'top'>): Record<string, string> {
  return {
    '--sent-left': `${Math.round(textBox.left - wrapper.left)}px`,
    '--sent-top': `${Math.round(textBox.top - wrapper.top)}px`,
    '--sent-width': `${Math.round(textBox.width)}px`,
    '--sent-height': `${Math.round(textBox.height)}px`,
  }
}

/** What the sent bubble says: the typed text, or — for a send of files alone — their
 *  names, so the bubble is never empty when something was sent. */
export function sentBubbleText(typed: string, attachmentNames: readonly string[]): string {
  const text = typed.trim()
  if (text) return text
  return attachmentNames.join(', ')
}
