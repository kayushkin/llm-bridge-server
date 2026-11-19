import { describe, expect, it } from 'vitest'
import type { SessionFile } from '@kayushkin/chat-core'
import {
  filesFromPaste,
  mayDrawAsImage,
  mayPlayAsVideo,
  messageWithAttachedFiles,
  nameForPastedFile,
  textPreviewKind,
} from '../src/sessionFileAttachments'

const shared = (path: string): SessionFile => ({
  file_id: 'file_000001',
  session_id: 'br_1',
  filename: 'a.png',
  media_type: 'image/png',
  size_bytes: 1,
  shared_by: 'user',
  path,
  created_at: '',
})

describe('messageWithAttachedFiles', () => {
  it('puts what was typed first and one path per file after it', () => {
    expect(messageWithAttachedFiles('  look at this  ', [shared('/f/1/a.png'), shared('/f/2/b.csv')])).toBe(
      'look at this\n\nAttached file: /f/1/a.png\nAttached file: /f/2/b.csv',
    )
  })
  it('is only the paths when nothing was typed', () => {
    expect(messageWithAttachedFiles('   ', [shared('/f/1/a.png')])).toBe('Attached file: /f/1/a.png')
  })
  it('is the typed text alone when nothing was attached', () => {
    expect(messageWithAttachedFiles('hi', [])).toBe('hi')
  })
})

describe('nameForPastedFile', () => {
  it('gives each paste its own name and keeps the extension', () => {
    const at = new Date('2026-09-27T12:34:56.789Z')
    expect(nameForPastedFile({ name: 'image.png', type: 'image/png' }, at)).toBe('pasted-2026-09-27T12-34-56Z.png')
    expect(nameForPastedFile({ name: 'blob', type: '' }, at)).toBe('pasted-2026-09-27T12-34-56Z')
  })
})

describe('mayDrawAsImage', () => {
  it('tries any image type and nothing else', () => {
    expect(mayDrawAsImage('image/webp')).toBe(true)
    expect(mayDrawAsImage('application/pdf')).toBe(false)
  })
})

describe('mayPlayAsVideo', () => {
  it('tries any video type and nothing else', () => {
    expect(mayPlayAsVideo('video/webm')).toBe(true)
    expect(mayPlayAsVideo('image/webp')).toBe(false)
  })
})

describe('filesFromPaste', () => {
  const png = new File(['x'], 'image.png', { type: 'image/png' })
  it('takes a screenshot and inserts no text', () => {
    expect(filesFromPaste({ files: [png], types: ['Files'] })).toEqual({ files: [png], keepText: false })
  })
  it('keeps the text of a paste that carried both', () => {
    expect(filesFromPaste({ files: [png], types: ['Files', 'text/plain'] }).keepText).toBe(true)
  })
  it('leaves a plain text paste alone', () => {
    expect(filesFromPaste({ files: [], types: ['text/plain'] })).toEqual({ files: [], keepText: true })
  })
})

describe('textPreviewKind', () => {
  it('reads a .md file as markdown even when the server typed it text/plain', () => {
    expect(textPreviewKind({ filename: 'week.md', media_type: 'text/plain' })).toBe('markdown')
    expect(textPreviewKind({ filename: 'NOTES.Markdown', media_type: 'text/plain; charset=utf-8' })).toBe('markdown')
  })
  it('reads text/markdown as markdown whatever the name', () => {
    expect(textPreviewKind({ filename: 'notes', media_type: 'text/markdown; charset=utf-8' })).toBe('markdown')
  })
  it('shows any other text file as plain text', () => {
    expect(textPreviewKind({ filename: 'log.txt', media_type: 'text/plain' })).toBe('plain')
    expect(textPreviewKind({ filename: 'rows.csv', media_type: 'text/csv' })).toBe('plain')
  })
  it('offers no preview for a file that is not text', () => {
    expect(textPreviewKind({ filename: 'a.png', media_type: 'image/png' })).toBeNull()
    expect(textPreviewKind({ filename: 'a.pdf', media_type: 'application/pdf' })).toBeNull()
  })
})
