// What the chat says about a file shared into a session, as pure rules so they are
// tested (test/sessionFileReferences.test.ts). The chip and its panel are
// components/chat/SessionFileChip.tsx; the cards that carry the chip are
// components/chat/SharedFiles.tsx.

import type { SessionFile } from '@kayushkin/chat-core'

/** The session and file a content address names. */
export interface SessionFileAddress {
  sessionId: string
  fileId: string
}

/**
 * The session file a link or image address points at, or null for any other address.
 *
 * An agent that wants a picture in its reply often pastes the address the chat reads
 * the bytes from — `…/sessions/<session id>/files/<file id>/content`, with or without
 * `?inline=true` — as a markdown image or link. That address names a record the chat
 * already holds, so the chat draws the record's chip there instead of a bare picture.
 * Only the path decides: whatever host and prefix the address carries, the two ids are
 * the last thing before `/content`.
 */
export function sessionFileAddressOf(address: string): SessionFileAddress | null {
  const path = address.split(/[?#]/, 1)[0] ?? ''
  const match = /(?:^|\/)sessions\/([^/]+)\/files\/([^/]+)\/content$/.exec(path)
  if (!match) return null
  try {
    return { sessionId: decodeURIComponent(match[1]!), fileId: decodeURIComponent(match[2]!) }
  } catch {
    // A malformed escape names nothing.
    return null
  }
}

/** How the chat shows a shared file: drawn, played, or only named. */
export type SessionFileKind = 'image' | 'video' | 'file'

export function sessionFileKind(mediaType: string): SessionFileKind {
  if (mediaType.startsWith('image/')) return 'image'
  if (mediaType.startsWith('video/')) return 'video'
  return 'file'
}

const GLYPH_BY_KIND: Record<SessionFileKind, string> = { image: '🖼', video: '🎬', file: '📄' }

export function sessionFileGlyph(kind: SessionFileKind): string {
  return GLYPH_BY_KIND[kind]
}

/** Who a file's card names as its sharer: the agent, or the person's name the
 *  transcript recorded for the share. */
export function sessionFileSharerText(file: Pick<SessionFile, 'shared_by'>, personName: string | null): string {
  if (file.shared_by === 'agent') return 'the agent'
  return personName || 'a person'
}

/** One line of a file's panel. `format` says how the value is drawn: as text, as a
 *  path in code, as a time, or as a session's own chip. */
export interface SessionFileFact {
  label: string
  value: string
  format: 'text' | 'path' | 'time' | 'session' | 'id'
}

/**
 * Everything the chat knows about a shared file, in the order the panel lists it:
 * what it is, who put it in which chat and when, then where it came from. A fact the
 * server did not record is left out, never guessed: a file shared before origins were
 * recorded has no "Shared from" line.
 */
export function sessionFileFacts(
  file: SessionFile,
  context: { personName: string | null; formatBytes: (bytes: number) => string },
): SessionFileFact[] {
  const facts: SessionFileFact[] = [
    { label: 'File id', value: file.file_id, format: 'id' },
    { label: 'Kind', value: `${file.media_type} · ${context.formatBytes(file.size_bytes)}`, format: 'text' },
    { label: 'Shared by', value: sessionFileSharerText(file, context.personName), format: 'text' },
    { label: 'Shared', value: file.created_at, format: 'time' },
    { label: 'In chat', value: file.session_id, format: 'session' },
  ]
  if (file.generation) {
    facts.push({ label: 'Made from the prompt', value: file.generation.prompt, format: 'text' })
    for (const reference of file.generation.reference_image_paths ?? []) {
      facts.push({ label: 'Reference image', value: reference, format: 'path' })
    }
    facts.push({ label: 'Made by', value: file.generation.instance_id, format: 'text' })
  }
  if (file.source_path) facts.push({ label: 'Shared from', value: file.source_path, format: 'path' })
  if (file.path) facts.push({ label: "Agent's copy", value: file.path, format: 'path' })
  return facts
}

/** What a folded group of pictures is called. */
export function imageGroupLabel(count: number): string {
  return `${count} images`
}
