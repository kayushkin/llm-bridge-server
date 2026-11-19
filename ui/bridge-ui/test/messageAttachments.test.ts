import { describe, expect, it } from 'vitest'
import {
  attachmentKind, captionsForFiles, humanFileSize, mediaCaption, mediaFileName, pendingAttachment,
} from '../src/messageAttachments'

describe('attachments', () => {
  it('sorts a file by its type', () => {
    expect(attachmentKind('image/png')).toBe('image')
    expect(attachmentKind('video/mp4')).toBe('video')
    expect(attachmentKind('audio/ogg')).toBe('audio')
    expect(attachmentKind('application/pdf')).toBe('file')
    expect(attachmentKind('')).toBe('file')
  })

  it('names a pasted screenshot by when it was pasted, and keeps a picked file’s name', () => {
    const now = new Date('2026-09-30T01:02:03.456Z')
    const screenshot = new File(['x'], 'image.png', { type: 'image/png' })
    expect(pendingAttachment(screenshot, true, now).name).toBe('pasted-2026-09-30T01-02-03Z.png')
    expect(pendingAttachment(screenshot, false, now).name).toBe('image.png')
    const a = pendingAttachment(screenshot, false, now)
    const b = pendingAttachment(screenshot, false, now)
    expect(a.id).not.toBe(b.id)
  })

  it('sends the typed text with the first file only', () => {
    expect(captionsForFiles('  look  ', 3)).toEqual(['look', '', ''])
    expect(captionsForFiles('', 1)).toEqual([''])
  })

  it('reads sizes', () => {
    expect(humanFileSize(512)).toBe('512 bytes')
    expect(humanFileSize(2048)).toBe('2 KB')
    expect(humanFileSize(5 * 1024 * 1024)).toBe('5.0 MB')
  })

  it('tells a caption from a file name', () => {
    expect(mediaCaption({ body: 'look at this', file_name: 'shot.png' })).toBe('look at this')
    expect(mediaCaption({ body: 'shot.png', file_name: 'shot.png' })).toBe('')
    expect(mediaCaption({ body: 'shot.png' })).toBe('')
    expect(mediaFileName({ body: 'look at this', file_name: 'shot.png' })).toBe('shot.png')
    expect(mediaFileName({ body: 'shot.png' })).toBe('shot.png')
  })
})
