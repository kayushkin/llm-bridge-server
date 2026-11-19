import { describe, expect, it } from 'vitest'
import {
  localPortsMentioned,
  previewsMentioned,
  previewsOfSession,
  previewUrl,
  workingDirectoryLabel,
  type DevServerPreview,
} from '../src/devServerPreviews'

function preview(port: number, previewPort?: number): DevServerPreview {
  return { port, listen_address: '::1', process_id: 1, command: 'vite', working_directory: '/home/u/repos/app', preview_port: previewPort }
}

describe('localPortsMentioned', () => {
  it('reads the port from what a dev server prints and what an agent curls', () => {
    const text = [
      '  VITE v6.4.3  ready in 236 ms',
      '  ➜  Local:   http://localhost:5173/',
      'curl -s http://127.0.0.1:8988/__login',
      'next dev on http://0.0.0.0:3000 and http://[::1]:4000/x',
      'ws://localhost:24678',
    ].join('\n')
    expect(localPortsMentioned(text)).toEqual([5173, 8988, 3000, 4000, 24678])
  })

  it('names a port once, and ignores hosts that are not this machine', () => {
    expect(localPortsMentioned('http://localhost:5173 http://localhost:5173/a https://example.com:8443 http://localhost.evil:80')).toEqual([5173])
  })

  it('does not take the start of a longer number as the port', () => {
    expect(localPortsMentioned('http://localhost:123456')).toEqual([])
  })
})

describe('previewsMentioned', () => {
  it('keeps only ports an agent process listens on, in the order the text names them', () => {
    const live = [preview(5173, 9100), preview(3000, 9101)]
    // 8191 is noteboard: a service of the host, never in the list.
    expect(previewsMentioned('curl localhost http://localhost:8191 http://localhost:3000 http://localhost:5173', live).map(p => p.port))
      .toEqual([3000, 5173])
  })
})

describe('previewUrl', () => {
  it('is the page hostname on the preview port', () => {
    expect(previewUrl(preview(5173, 9101), { protocol: 'https:', hostname: 'dash.kayushkin.com' })).toBe('https://dash.kayushkin.com:9101/')
  })

  it('is null when every preview port is taken', () => {
    expect(previewUrl(preview(5173), { protocol: 'https:', hostname: 'dash.kayushkin.com' })).toBeNull()
  })
})

describe('workingDirectoryLabel', () => {
  it('is the last segment', () => {
    expect(workingDirectoryLabel('/home/u/repos/dash-wt-previews/')).toBe('dash-wt-previews')
    expect(workingDirectoryLabel('/')).toBe('/')
  })
})

describe('previewsOfSession', () => {
  it('keeps the previews the host says that session ran', () => {
    const session = (id: string) => ({ session_id: id, event_count: 1, last_active: '2026-09-25T19:59:37Z' })
    const mine = { ...preview(5173, 9100), sessions: [session('br_1'), session('br_2')] }
    const theirs = { ...preview(3000, 9101), sessions: [session('br_3')] }
    const unknown = preview(8080, 9102)
    expect(previewsOfSession([mine, theirs, unknown], 'br_2')).toEqual([mine])
  })
})
