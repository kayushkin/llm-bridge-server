import { describe, expect, it } from 'vitest'
import { rowsQueryString } from '../src/servicesClient'

// The query string is the contract with llm-bridge-server's rows route: a
// filter is `column:op:value`, split by the server on the first two colons
// only, and a null-ness filter carries no value segment at all.
describe('rowsQueryString', () => {
  it('encodes order, limit and every filter, keeping colons inside a value', () => {
    const qs = rowsQueryString({
      path: '/home/u/.config/x/x.db', table: 'jobs', limit: 50, orderBy: 'created_at', descending: true,
      filters: [
        { column: 'created_at', op: 'gte', value: '2026-09-10T00:00:00Z' },
        { column: 'error', op: 'not_null', value: 'ignored' },
      ],
    })
    const params = new URLSearchParams(qs)
    expect(params.get('path')).toBe('/home/u/.config/x/x.db')
    expect(params.get('table')).toBe('jobs')
    expect(params.get('limit')).toBe('50')
    expect(params.get('order')).toBe('desc')
    expect(params.get('order_by')).toBe('created_at')
    expect(params.getAll('filter')).toEqual(['created_at:gte:2026-09-10T00:00:00Z', 'error:not_null'])
  })

  it('sends no order_by when the caller named none, so the server picks rowid', () => {
    const params = new URLSearchParams(rowsQueryString({ path: 'p', table: 't', limit: 20, orderBy: '', descending: false, filters: [] }))
    expect(params.has('order_by')).toBe(false)
    expect(params.get('order')).toBe('asc')
    expect(params.getAll('filter')).toEqual([])
  })
})
