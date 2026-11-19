import { describe, expect, it } from 'vitest'
import type { Entry } from '@kayushkin/chat-core'
import { heldMessageOutcomes, heldMessageRow } from '../src/heldMessages'

function systemEntry(id: string, subtype: string, heldMessageId: string, text?: string): Entry {
  return { id, kind: 'system', role: 'system', subtype, heldMessageId, text, ts: '2026-11-03T14:00:00-08:00' } as unknown as Entry
}

describe('a message the server held before a restart', () => {
  const held = systemEntry('1', 'message_held_for_restart', 'held_A', 'please continue')

  it('waits while nothing says how it ended', () => {
    const entries = { '1': held }
    expect(heldMessageRow(held, () => heldMessageOutcomes(entries))).toEqual({})
  })

  it('is hidden once sent, because its own message is then the row', () => {
    const entries = { '1': held, '9': systemEntry('9', 'held_message_delivered', 'held_A') }
    expect(heldMessageRow(held, () => heldMessageOutcomes(entries))).toBe('hidden')
  })

  it('stays, with the reason, when sending it failed', () => {
    const entries = { '1': held, '9': systemEntry('9', 'held_message_delivery_failed', 'held_A', 'the send answered 402') }
    expect(heldMessageRow(held, () => heldMessageOutcomes(entries))).toEqual({ failedBecause: 'the send answered 402' })
  })

  it('pairs outcomes by held message id, not by order', () => {
    const entries = { '1': held, '9': systemEntry('9', 'held_message_delivered', 'held_B') }
    expect(heldMessageRow(held, () => heldMessageOutcomes(entries))).toEqual({})
  })

  it('does not read the session for an entry that is not a held message', () => {
    const other = systemEntry('2', 'compact_boundary', '')
    expect(heldMessageRow(other, () => { throw new Error('scanned the session') })).toBeNull()
  })
})
