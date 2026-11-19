import { describe, expect, it } from 'vitest'
import { baseNameOf, imageNamedByMention } from '../src/sessionImageMentions'

const images = [
  { key: 'file:a', name: 'home.png', sharedAt: '2026-01-01T10:00:00Z' },
  { key: 'file:b', name: 'home.png', sharedAt: '2026-01-01T12:00:00Z' },
  { key: 'file:c', name: 'member.png', sharedAt: '2026-01-01T11:00:00Z' },
  { key: 'tool:e1:0', name: 'Read image 1' },
]

describe('imageNamedByMention', () => {
  it('matches on the last path segment', () => {
    expect(imageNamedByMention(images, 'member.png', null)?.key).toBe('file:c')
    expect(imageNamedByMention(images, '/tmp/shots/member.png', null)?.key).toBe('file:c')
  })

  it('picks the newest shared no later than the message', () => {
    expect(imageNamedByMention(images, 'home.png', '2026-01-01T11:00:00Z')?.key).toBe('file:a')
    expect(imageNamedByMention(images, 'home.png', '2026-01-01T13:00:00Z')?.key).toBe('file:b')
  })

  it('falls to the newest when the message has no time or predates every copy', () => {
    expect(imageNamedByMention(images, 'home.png', null)?.key).toBe('file:b')
    expect(imageNamedByMention(images, 'home.png', '2025-12-31T00:00:00Z')?.key).toBe('file:b')
  })

  it('never matches a tool image, a partial name or a directory', () => {
    expect(imageNamedByMention(images, 'Read image 1', null)).toBeNull()
    expect(imageNamedByMention(images, 'ome.png', null)).toBeNull()
    expect(imageNamedByMention(images, 'shots/', null)).toBeNull()
  })
})

describe('baseNameOf', () => {
  it('drops the directory', () => {
    expect(baseNameOf('/a/b/c.png')).toBe('c.png')
    expect(baseNameOf('c.png')).toBe('c.png')
  })
})
