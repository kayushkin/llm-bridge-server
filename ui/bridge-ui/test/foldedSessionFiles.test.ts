import { describe, expect, it } from 'vitest'
import {
  imageGroupFoldKey,
  parseFoldedSessionFiles,
  sessionFileFoldKey,
  withSessionFileFold,
} from '../src/foldedSessionFiles'

describe('parseFoldedSessionFiles', () => {
  it('reads the stored list', () => {
    expect(parseFoldedSessionFiles('["file:file_000001","group:file_000002"]')).toEqual(['file:file_000001', 'group:file_000002'])
  })

  it('reads nothing stored, and anything that is not a list of keys, as no folds', () => {
    expect(parseFoldedSessionFiles(null)).toEqual([])
    expect(parseFoldedSessionFiles('')).toEqual([])
    expect(parseFoldedSessionFiles('not json')).toEqual([])
    expect(parseFoldedSessionFiles('{"file:file_000001":true}')).toEqual([])
    expect(parseFoldedSessionFiles('["file:file_000001",7,null,""]')).toEqual(['file:file_000001'])
  })
})

describe('withSessionFileFold', () => {
  it('adds a fold last and takes one away', () => {
    expect(withSessionFileFold(['a'], 'b', true)).toEqual(['a', 'b'])
    expect(withSessionFileFold(['a', 'b'], 'a', false)).toEqual(['b'])
  })

  it('keeps one row for a key folded twice, as the newest', () => {
    expect(withSessionFileFold(['a', 'b'], 'a', true)).toEqual(['b', 'a'])
  })

  it('unfolding a key that is not folded changes nothing', () => {
    expect(withSessionFileFold(['a'], 'b', false)).toEqual(['a'])
  })

  it('forgets the oldest folds past the limit', () => {
    expect(withSessionFileFold(['a', 'b', 'c'], 'd', true, 3)).toEqual(['b', 'c', 'd'])
  })
})

describe('fold keys', () => {
  it("keeps one file's fold apart from the fold of the group that file leads", () => {
    expect(sessionFileFoldKey('file_000001')).not.toBe(imageGroupFoldKey('file_000001'))
  })
})
