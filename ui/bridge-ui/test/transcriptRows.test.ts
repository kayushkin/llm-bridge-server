import { describe, expect, it } from 'vitest'
import { transcriptRows, type RenderItem, type TranscriptRow } from '../src/transcriptRows'

const user = (key: string): RenderItem => ({
  kind: 'user', key, ts: '2026-01-01T10:00:00Z', text: 'a question', entryIds: [key], actor: 'Priya Raman',
})
const assistant = (key: string, prose = 'an answer'): RenderItem => ({
  kind: 'assistant', key, ts: '2026-01-01T10:00:05Z', prose, narration: '', thinking: '', errors: [], entryIds: [key],
})
const notification = (key: string): RenderItem => ({
  kind: 'notification', key, ts: '2026-01-01T10:05:00Z', text: '<task-notification>done</task-notification>',
})
const file = (key: string, sharedByAgent: boolean): RenderItem => ({
  kind: 'file',
  key,
  ts: '2026-01-01T10:00:09Z',
  file: {
    file_id: `file_${key}`,
    session_id: 'br_1',
    filename: 'picture.png',
    media_type: 'image/png',
    size_bytes: 10,
    shared_by: sharedByAgent ? 'agent' : 'user',
    created_at: '2026-01-01T10:00:09Z',
  } as Extract<RenderItem, { kind: 'file' }>['file'],
  sharedByAgent,
  actor: 'Priya Raman',
})

const keysOf = (rows: TranscriptRow[]) => rows.map((row) => row.key)
const spanItemKeys = (row: TranscriptRow | undefined) =>
  row?.kind === 'span' ? row.segments.map((segment) => segment.items.map((item) => item.key)) : null

describe('transcriptRows', () => {
  it('draws a turn as the person\'s message and one agent row', () => {
    const rows = transcriptRows([{ turnId: 'turn_1', items: [user('e_1'), assistant('msg:a'), assistant('msg:b')] }], false)
    expect(keysOf(rows)).toEqual(['e_1', 'span:turn_1'])
    expect(spanItemKeys(rows[1])).toEqual([['msg:a', 'msg:b']])
  })

  it('keeps the agent\'s shared file inside its row and gives a person\'s file its own', () => {
    const rows = transcriptRows(
      [{ turnId: 'turn_1', items: [user('e_1'), file('e_2', true), assistant('msg:a'), file('e_3', false)] }],
      false,
    )
    expect(rows.map((row) => row.kind)).toEqual(['user', 'span', 'file'])
    expect(spanItemKeys(rows[1])).toEqual([['e_2', 'msg:a']])
  })

  // The fault this module exists to prevent. A file a person shares carries no turn
  // id, so it joins the turn that is running; what the agent writes after it in that
  // turn is a second agent row begun in the same turn. Both rows were keyed
  // `span:<turn id>`, and React keeps the first of two same-keyed rows in the page
  // when the list's rows are next replaced: it stayed, at its old offset, over the
  // next session opened.
  it('gives each agent row its own key when a person\'s file lands in the middle of a turn', () => {
    const rows = transcriptRows(
      [{ turnId: 'turn_1', items: [user('e_1'), assistant('msg:a'), file('e_2', false), assistant('msg:b')] }],
      false,
    )
    expect(rows.map((row) => row.kind)).toEqual(['user', 'span', 'file', 'span'])
    expect(keysOf(rows)).toEqual(['e_1', 'span:turn_1', 'e_2', 'span:turn_1:2'])
    expect(new Set(keysOf(rows)).size).toBe(rows.length)
  })

  it('gives each agent row its own key when a person\'s message lands in the middle of a turn', () => {
    const rows = transcriptRows(
      [
        {
          turnId: 'turn_1',
          items: [user('e_1'), assistant('msg:a'), user('e_2'), assistant('msg:b'), user('e_3'), assistant('msg:c')],
        },
      ],
      false,
    )
    expect(keysOf(rows)).toEqual(['e_1', 'span:turn_1', 'e_2', 'span:turn_1:2', 'e_3', 'span:turn_1:3'])
  })

  it('gives two rows of one entry two keys', () => {
    const rows = transcriptRows(
      [
        { turnId: 'turn_1', items: [user('e_1'), assistant('msg:a')] },
        { turnId: 'turn_2', items: [user('e_1'), file('e_2', false), file('e_2', false)] },
      ],
      false,
    )
    expect(keysOf(rows)).toEqual(['e_1', 'span:turn_1', 'e_1:2', 'e_2', 'e_2:2'])
  })

  it('keeps the first agent row\'s key while the turn grows past a file', () => {
    const before = transcriptRows([{ turnId: 'turn_1', items: [user('e_1'), assistant('msg:a')] }], true)
    const after = transcriptRows(
      [{ turnId: 'turn_1', items: [user('e_1'), assistant('msg:a'), file('e_2', false), assistant('msg:b')] }],
      true,
    )
    expect(before[1]?.key).toBe('span:turn_1')
    expect(after[1]?.key).toBe('span:turn_1')
  })

  it('continues one agent row through a turn a notification opened', () => {
    const rows = transcriptRows(
      [
        { turnId: 'turn_1', items: [user('e_1'), assistant('msg:a')] },
        { turnId: 'turn_2', items: [notification('e_2'), assistant('msg:b')] },
        { turnId: 'turn_3', items: [user('e_3'), assistant('msg:c')] },
      ],
      false,
    )
    expect(keysOf(rows)).toEqual(['e_1', 'span:turn_1', 'e_3', 'span:turn_3'])
    expect(spanItemKeys(rows[1])).toEqual([['msg:a'], ['msg:b']])
    const continued = rows[1]
    expect(continued?.kind === 'span' && continued.segments[1]?.notification).toBe('<task-notification>done</task-notification>')
  })

  it('keys a row begun mid-turn apart from the row that turn continued', () => {
    // turn_2 continues turn_1's row, then a file ends it and the agent writes on.
    const rows = transcriptRows(
      [
        { turnId: 'turn_1', items: [user('e_1'), assistant('msg:a')] },
        { turnId: 'turn_2', items: [notification('e_2'), assistant('msg:b'), file('e_3', false), assistant('msg:c')] },
      ],
      false,
    )
    expect(keysOf(rows)).toEqual(['e_1', 'span:turn_1', 'e_3', 'span:turn_2'])
    expect(new Set(keysOf(rows)).size).toBe(rows.length)
  })

  it('marks only the row holding the last turn\'s last answer as streaming', () => {
    const turns = [
      { turnId: 'turn_1', items: [user('e_1'), assistant('msg:a')] },
      { turnId: 'turn_2', items: [user('e_2'), assistant('msg:b'), file('e_3', false), assistant('msg:c')] },
    ]
    const streaming = (rows: TranscriptRow[]) => rows.map((row) => (row.kind === 'span' ? row.streaming : null))
    expect(streaming(transcriptRows(turns, true))).toEqual([null, false, null, false, null, true])
    expect(streaming(transcriptRows(turns, false))).toEqual([null, false, null, false, null, false])
  })
})
