import { describe, expect, it } from 'vitest'
import type { SessionFile } from '@kayushkin/chat-core'
import {
  imageGroupLabel,
  sessionFileAddressOf,
  sessionFileFacts,
  sessionFileGlyph,
  sessionFileKind,
  sessionFileSharerText,
} from '../src/sessionFileReferences'

const formatBytes = (bytes: number) => `${bytes} B`

function sharedFile(overrides: Partial<SessionFile> = {}): SessionFile {
  return {
    file_id: 'file_000042',
    session_id: 'br_1700000000000000001',
    filename: 'hood.png',
    media_type: 'image/png',
    size_bytes: 2048,
    shared_by: 'agent',
    path: '/srv/session-files/br_1700000000000000001/file_000042/hood.png',
    created_at: '2026-11-17T23:12:28Z',
    ...overrides,
  }
}

describe('sessionFileAddressOf', () => {
  it('reads the session and file out of the address the chat draws a picture from', () => {
    expect(sessionFileAddressOf('/api/bridge/sessions/br_1700000000000000001/files/file_000042/content?inline=true')).toEqual({
      sessionId: 'br_1700000000000000001',
      fileId: 'file_000042',
    })
  })

  it('reads the download address, with no query', () => {
    expect(sessionFileAddressOf('/api/bridge/sessions/br_1/files/file_000007/content')).toEqual({ sessionId: 'br_1', fileId: 'file_000007' })
  })

  it('reads it whatever host and prefix come before', () => {
    expect(sessionFileAddressOf('https://chat.example/bridge/sessions/br_1/files/file_000007/content#top')).toEqual({
      sessionId: 'br_1',
      fileId: 'file_000007',
    })
    expect(sessionFileAddressOf('sessions/br_1/files/file_000007/content')).toEqual({ sessionId: 'br_1', fileId: 'file_000007' })
  })

  it('decodes an escaped id', () => {
    expect(sessionFileAddressOf('/sessions/herald%2Dx/files/file_000007/content')?.sessionId).toBe('herald-x')
  })

  it('answers null for any other address', () => {
    for (const address of [
      '',
      'https://example.com/picture.png',
      '/api/bridge/sessions/br_1/files',
      '/api/bridge/sessions/br_1/files/file_000007',
      '/api/bridge/sessions/br_1/files/file_000007/content/extra',
      '/api/bridge/sessions/br_1/entries/12/images/0',
      '/subsessions/br_1/files/file_000007/content',
      '/sessions/%E0%A4%A/files/file_000007/content',
    ]) {
      expect(sessionFileAddressOf(address), address).toBeNull()
    }
  })
})

describe('sessionFileKind', () => {
  it('tells a picture, a video and any other file apart by media type', () => {
    expect(sessionFileKind('image/jpeg')).toBe('image')
    expect(sessionFileKind('video/mp4')).toBe('video')
    expect(sessionFileKind('application/zip')).toBe('file')
    expect(sessionFileKind('text/markdown')).toBe('file')
  })

  it('gives each kind its own glyph', () => {
    const glyphs = (['image', 'video', 'file'] as const).map(sessionFileGlyph)
    expect(new Set(glyphs).size).toBe(3)
  })
})

describe('sessionFileSharerText', () => {
  it('names the agent for a file the agent shared, whatever person label came along', () => {
    expect(sessionFileSharerText({ shared_by: 'agent' }, 'Dana')).toBe('the agent')
  })

  it("names the person by the transcript's label, and says so when there is none", () => {
    expect(sessionFileSharerText({ shared_by: 'user' }, 'Dana')).toBe('Dana')
    expect(sessionFileSharerText({ shared_by: 'user' }, null)).toBe('a person')
  })
})

describe('sessionFileFacts', () => {
  it('lists the id first, then what it is, who shared it into which chat and when', () => {
    const facts = sessionFileFacts(sharedFile(), { personName: null, formatBytes })
    expect(facts.map((fact) => fact.label)).toEqual(['File id', 'Kind', 'Shared by', 'Shared', 'In chat', "Agent's copy"])
    expect(facts[0]).toEqual({ label: 'File id', value: 'file_000042', format: 'id' })
    expect(facts[1]!.value).toBe('image/png · 2048 B')
    expect(facts[2]!.value).toBe('the agent')
    expect(facts[3]).toEqual({ label: 'Shared', value: '2026-11-17T23:12:28Z', format: 'time' })
    expect(facts[4]).toEqual({ label: 'In chat', value: 'br_1700000000000000001', format: 'session' })
  })

  it('says where an agent took the file from when the server recorded it', () => {
    const facts = sessionFileFacts(sharedFile({ source_path: '/work/renders/hood.png' }), { personName: null, formatBytes })
    expect(facts).toContainEqual({ label: 'Shared from', value: '/work/renders/hood.png', format: 'path' })
  })

  it('leaves the origin out when none was recorded, rather than guessing one', () => {
    const labels = sessionFileFacts(sharedFile(), { personName: null, formatBytes }).map((fact) => fact.label)
    expect(labels).not.toContain('Shared from')
    expect(labels).not.toContain('Made from the prompt')
  })

  it('gives the prompt, each reference image and the instance of a generated image', () => {
    const facts = sessionFileFacts(
      sharedFile({
        source_path: '/work/images/crab.png',
        generation: { prompt: 'a red crab in a navy beret', reference_image_paths: ['/work/a.png', '/work/b.png'], instance_id: 'inst-image' },
      }),
      { personName: null, formatBytes },
    )
    expect(facts.filter((fact) => fact.label === 'Reference image').map((fact) => fact.value)).toEqual(['/work/a.png', '/work/b.png'])
    expect(facts).toContainEqual({ label: 'Made from the prompt', value: 'a red crab in a navy beret', format: 'text' })
    expect(facts).toContainEqual({ label: 'Made by', value: 'inst-image', format: 'text' })
  })

  it('names the person who uploaded a file', () => {
    const facts = sessionFileFacts(sharedFile({ shared_by: 'user' }), { personName: 'Dana', formatBytes })
    expect(facts.find((fact) => fact.label === 'Shared by')!.value).toBe('Dana')
  })
})

describe('imageGroupLabel', () => {
  it('counts the pictures', () => {
    expect(imageGroupLabel(6)).toBe('6 images')
  })
})
