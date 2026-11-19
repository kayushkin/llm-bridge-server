import { describe, expect, it } from 'vitest'
import {
  renderChangedSinceViewed,
  sessionTree,
  shortFileLabel,
  threeDimensionalModelFileUrl,
  type ThreeDimensionalModelSession,
} from '../src/threeDimensionalModels'

function session(id: string, root?: string): ThreeDimensionalModelSession {
  return {
    session_id: id, display_name: id, harness: 'codex', state: 'idle', purpose: 'delegate',
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    root_session_id: root, folders: [], models: [], renders: [],
  }
}

describe('sessionTree', () => {
  it('puts a session under its listed root and keeps an unlisted root as its own row', () => {
    const tree = sessionTree([session('br_child_1', 'br_root'), session('br_root'), session('br_orphan', 'br_gone'), session('br_child_2', 'br_root')])
    expect(tree.map(node => node.session.session_id)).toEqual(['br_root', 'br_orphan'])
    expect(tree[0].children.map(child => child.session_id)).toEqual(['br_child_1', 'br_child_2'])
    expect(tree[1].children).toEqual([])
  })

  it('treats a session that is its own root as top level', () => {
    const tree = sessionTree([session('br_root', 'br_root')])
    expect(tree).toHaveLength(1)
  })
})

describe('threeDimensionalModelFileUrl', () => {
  it('escapes the path and carries the file time so a rewrite is fetched again', () => {
    expect(threeDimensionalModelFileUrl('/api/3d-models', '/work/a b/#1.glb', '2026-01-01T00:00:00Z'))
      .toBe('/api/3d-models/file?path=%2Fwork%2Fa+b%2F%231.glb&modified=2026-01-01T00%3A00%3A00Z')
  })
})

describe('shortFileLabel', () => {
  it('keeps the folder above the file', () => {
    expect(shortFileLabel('/art/kingfisher/hoods/hybrid.glb', '/art/kingfisher')).toBe('hoods/hybrid.glb')
    expect(shortFileLabel('/art/kingfisher/top.glb', '/art/kingfisher')).toBe('top.glb')
  })
})

describe('renderChangedSinceViewed', () => {
  it('says so only when the file on disk is newer than the last look', () => {
    const base = { path: '/a.png', viewed_at: [], last_viewed_at: '2026-01-01T00:00:10Z', tool: 'image_view', exists: true }
    expect(renderChangedSinceViewed({ ...base, modified_at: '2026-01-01T00:00:05Z' })).toBe(false)
    expect(renderChangedSinceViewed({ ...base, modified_at: '2026-01-01T00:01:00Z' })).toBe(true)
    expect(renderChangedSinceViewed({ ...base, exists: false })).toBe(false)
  })
})
