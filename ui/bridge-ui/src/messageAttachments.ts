// Files attached in the Conversations composer — pasted, dropped or picked —
// and how a sent file is shown: what kind each one is, which one carries the
// typed text as its caption, and how a file message's caption is told from its
// name. Pasting reuses the chat's rules (sessionFileAttachments.ts). Covered
// by test/messageAttachments.test.ts.
import { nameForPastedFile } from './sessionFileAttachments'

/** How a file is shown before it is sent and after: the same split multichat
 *  makes when it picks the Matrix msgtype. */
export type AttachmentKind = 'image' | 'video' | 'audio' | 'file'

export function attachmentKind(mimeType: string): AttachmentKind {
  if (mimeType.startsWith('image/')) return 'image'
  if (mimeType.startsWith('video/')) return 'video'
  if (mimeType.startsWith('audio/')) return 'audio'
  return 'file'
}

/** A file waiting in the composer. `name` is what it is sent as: a pasted
 *  screenshot gets a stamped name, since every browser calls it image.png. */
export interface PendingAttachment {
  id: string
  file: File
  name: string
  kind: AttachmentKind
}

let nextAttachmentID = 0

export function pendingAttachment(file: File, pasted: boolean, now: Date): PendingAttachment {
  nextAttachmentID += 1
  return {
    id: `attachment-${nextAttachmentID}`,
    file,
    name: pasted ? nameForPastedFile(file, now) : file.name,
    kind: attachmentKind(file.type),
  }
}

/** The caption each file is sent with: the typed text goes with the first
 *  file, so on Discord text and file arrive as one message; the rest go
 *  without. */
export function captionsForFiles(text: string, fileCount: number): string[] {
  const caption = text.trim()
  return Array.from({ length: fileCount }, (_, index) => (index === 0 ? caption : ''))
}

/** A file's size in the units a person reads. */
export function humanFileSize(bytes: number): string {
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MB`
  if (bytes >= 1 << 10) return `${Math.round(bytes / (1 << 10))} KB`
  return `${bytes} bytes`
}

/** A file message's caption: its body when the sender named the file apart
 *  from it (MSC2530), else "" — without a file name, body is the name. */
export function mediaCaption(message: { body: string; file_name?: string }): string {
  if (!message.file_name || message.body === message.file_name) return ''
  return message.body.trim()
}

/** What a file message is called: its file name when it has a caption,
 *  else its body. */
export function mediaFileName(message: { body: string; file_name?: string }): string {
  return message.file_name || message.body
}
