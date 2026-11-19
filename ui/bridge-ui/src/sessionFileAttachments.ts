import type { SessionFile } from '@kayushkin/chat-core'

/**
 * The message a send carries when files were attached to it.
 *
 * The agent cannot see the chat's file cards — those are drawn from `session_file`
 * events, which reach the browser and never the harness. What reaches the agent is the
 * text, so the text names where each file is: the path llm-bridge-server copied it to,
 * which the agent reads like any other file. One line per file, after what the person
 * typed, so the words they wrote stay first.
 */
export function messageWithAttachedFiles(draft: string, shared: readonly SessionFile[]): string {
  const typed = draft.trim()
  if (shared.length === 0) return typed
  const lines = shared.map((file) => `Attached file: ${file.path}`)
  return typed ? `${typed}\n\n${lines.join('\n')}` : lines.join('\n')
}

/**
 * The name a pasted file is shared under.
 *
 * A browser names every pasted screenshot `image.png`, so three pastes would read as
 * three copies of one file in the chat and in the agent's message. A picked or dropped
 * file keeps the name it had.
 */
export function nameForPastedFile(file: { name: string; type: string }, pastedAt: Date): string {
  const extension = file.name.includes('.') ? file.name.slice(file.name.lastIndexOf('.')) : ''
  const stamp = pastedAt.toISOString().replace(/[:]/g, '-').replace(/\.\d+Z$/, 'Z')
  return `pasted-${stamp}${extension}`
}

/**
 * Whether a shared file is worth trying to draw as an image. Only a try: the server
 * decides what it serves inline (file-store's `inline_content_types`, log-store's image
 * route), and a type it serves as a download fails to draw — the card then falls back
 * to a download link. Asking the type here rather than keeping a list of types keeps
 * that list in one place, the server's.
 */
export function mayDrawAsImage(mediaType: string): boolean {
  return mediaType.startsWith('image/')
}

/** Whether a shared file is worth trying to play as a video — a recording of a browser
 *  run, most often. The same kind of try as `mayDrawAsImage`: a type the server serves
 *  only as a download fails to play, and the card falls back to the link. */
export function mayPlayAsVideo(mediaType: string): boolean {
  return mediaType.startsWith('video/')
}

/** Files a paste carried, and whether the paste should still insert its text.
 *
 *  A screenshot pastes as a file and no text. Copying an image from a web page pastes
 *  the image AND text (its address, or alt text); the text is what the person may have
 *  meant, so it is kept, and the image is attached as well. */
export function filesFromPaste(clipboard: {
  files: ArrayLike<File>
  types: readonly string[]
}): { files: File[]; keepText: boolean } {
  const files = Array.from(clipboard.files)
  return { files, keepText: files.length === 0 || clipboard.types.includes('text/plain') }
}

/** How a shared file's text can be shown in the chat, or null when it cannot.
 *
 *  Markdown is told by name as well as type: llm-bridge-server types a `.md` file
 *  `text/plain`, so the type alone would show every markdown file as raw source. Any
 *  other `text/*` file is shown as it is. */
export type TextPreviewKind = 'markdown' | 'plain'

export function textPreviewKind(file: { filename: string; media_type: string }): TextPreviewKind | null {
  const mediaType = file.media_type.split(';')[0].trim().toLowerCase()
  if (mediaType === 'text/markdown' || /\.(md|markdown)$/i.test(file.filename)) return 'markdown'
  if (mediaType.startsWith('text/')) return 'plain'
  return null
}
