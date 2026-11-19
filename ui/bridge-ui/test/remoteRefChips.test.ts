import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { ChatContext } from '@kayushkin/chat-core'
import { RefChip } from '../src/components/chat/RefChip'
import {
  RefChipServerContext,
  refChipRoute,
  sessionLookupWasNotFound,
  type RefChipServer,
} from '../src/components/chat/refChipServer'

// A chip in a connected server's thread names that server's record. Ids are
// per server, so nothing about it may be asked of this server: an answer would
// be about a different record, given as if it were the same one.

const laptop: RefChipServer = { connectionId: 'peer_connection_000001', serverName: 'laptop' }

describe('which chip a reference draws', () => {
  it('is this server’s own chip outside a connected server’s thread', () => {
    for (const kind of ['session', 'uuid', 'registered', 'note', 'todo', 'file', 'action']) {
      expect(refChipRoute(kind, null)).toBe('local')
    }
  })

  it('loads a session through the connected server, looks a record up on that server, and draws the rest as that server’s', () => {
    expect(refChipRoute('session', laptop)).toBe('remote-session')
    for (const kind of ['uuid', 'registered', 'note', 'todo']) {
      expect(refChipRoute(kind, laptop)).toBe('remote-lookup')
    }
    for (const kind of ['rank', 'workspace', 'file', 'action', 'image']) {
      expect(refChipRoute(kind, laptop)).toBe('remote-neutral')
    }
  })
})

describe('a session this server does not have', () => {
  it('is told apart from a failure to ask', () => {
    expect(sessionLookupWasNotFound('GET /sessions/br_1 failed: 404 Not Found')).toBe(true)
    expect(sessionLookupWasNotFound('GET /sessions/br_1 failed: 502 Bad Gateway')).toBe(false)
    expect(sessionLookupWasNotFound('Failed to fetch')).toBe(false)
    expect(sessionLookupWasNotFound(null)).toBe(false)
  })
})

describe('a connected server’s id that this server cannot look up', () => {
  // Every client this server would ask throws if it is called at all.
  const refuse = (what: string) => () => { throw new Error(`${what} must not be asked about a connected server’s id`) }
  const localLookups = {
    api: new Proxy({}, { get: (_target, name) => refuse(`api.${String(name)}`) }),
    noteboard: { getItem: refuse('noteboard') },
    resolve: { resolve: refuse('resolver') },
  }
  const inLaptopThread = (kind: string, refId: string) =>
    renderToStaticMarkup(
      createElement(ChatContext.Provider, { value: localLookups as never },
        createElement(RefChipServerContext.Provider, { value: laptop },
          createElement(RefChip, { kind, refId }))))

  for (const [kind, refId] of [
    ['registered', 'principal_000014'],
    ['uuid', '8637db10-0000-4000-8000-000000000000'],
    ['todo', '8637db10-0000-4000-8000-000000000001'],
  ]) {
    it(`draws ${kind} ${refId} as from the connected server until that server answers, asking nothing here`, () => {
      const html = inLaptopThread(kind, refId)
      expect(html).toContain('ref-chip-remote')
      expect(html).toContain(`data-ref-id="${refId}"`)
      expect(html).toContain('data-ref-server="laptop"')
      expect(html).toContain('this server cannot look it up')
    })
  }
})
