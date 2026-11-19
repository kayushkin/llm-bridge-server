// The unicode emoji table's shape, and how a picked emoji goes into the text
// box. The table itself is src/emojiData.ts, generated from Unicode's data and
// loaded only when a picker first opens; searching it is emojiCatalog.ts.

export interface EmojiGroup {
  name: string
  /** [emoji, name], and 1 third when the emoji takes a skin tone. */
  emojis: readonly (readonly [string, string, 1?])[]
}

/** The draft with `insert` put in place of the selection, and where the cursor
 *  goes: just after what went in. A selection past the end (the draft changed
 *  under it) is clamped to the end. */
export function insertAtSelection(draft: string, selectionStart: number, selectionEnd: number, insert: string): { text: string; cursor: number } {
  const start = Math.min(Math.max(0, selectionStart), draft.length)
  const end = Math.min(Math.max(start, selectionEnd), draft.length)
  return { text: draft.slice(0, start) + insert + draft.slice(end), cursor: start + insert.length }
}
