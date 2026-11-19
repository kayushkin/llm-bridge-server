import { describe, expect, it } from 'vitest'
import {
  bridgeRefusalText,
  bridgeSignInPaths,
  fetchWithBridgeCsrfToken,
  logOutOfBridge,
  methodNeedsCsrfToken,
  readBridgeSignIn,
  signInWithDemoLogin,
} from '../src/bridgeSignIn'

interface RecordedCall { url: string; init: RequestInit }

function recordingFetch(response: () => Response) {
  const calls: RecordedCall[] = []
  const fetchImpl = (async (input: RequestInfo | URL, init: RequestInit = {}) => {
    calls.push({ url: String(input), init })
    return response()
  }) as typeof fetch
  return { calls, fetchImpl }
}

const signIn = {
  principal_id: 'principal_000001',
  expires_at: '2026-11-12T00:00:00Z',
  login_session_id: 'login_session_000001',
  method: 'demo',
  csrf_token: 'csrf-made-up',
}

describe('bridge sign-in', () => {
  it('names the routes under the base path', () => {
    const paths = bridgeSignInPaths('/api/bridge')
    expect(paths.principal).toBe('/api/bridge/auth/principal')
    expect(paths.demoLogin).toBe('/api/bridge/auth/demo-login')
    expect(paths.logout).toBe('/api/bridge/auth/logout')
    expect(paths.entraStart('/kanban?x=1')).toBe('/api/bridge/auth/entra/start?return_to=%2Fkanban%3Fx%3D1')
  })

  it('needs the CSRF token on every method but GET, HEAD and OPTIONS', () => {
    expect(methodNeedsCsrfToken(undefined)).toBe(false)
    expect(methodNeedsCsrfToken('get')).toBe(false)
    expect(methodNeedsCsrfToken('HEAD')).toBe(false)
    expect(methodNeedsCsrfToken('OPTIONS')).toBe(false)
    for (const method of ['POST', 'put', 'PATCH', 'DELETE']) expect(methodNeedsCsrfToken(method)).toBe(true)
  })

  it('adds the token to a write and leaves a read alone, keeping the caller\'s headers', async () => {
    const { calls, fetchImpl } = recordingFetch(() => new Response('{}'))
    const withToken = fetchWithBridgeCsrfToken('csrf-made-up', fetchImpl)
    await withToken('/api/bridge/sessions')
    await withToken('/api/bridge/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' } })
    expect(calls[0].init.credentials).toBe('include')
    expect(new Headers(calls[0].init.headers).get('X-CSRF-Token')).toBeNull()
    const writeHeaders = new Headers(calls[1].init.headers)
    expect(writeHeaders.get('X-CSRF-Token')).toBe('csrf-made-up')
    expect(writeHeaders.get('Content-Type')).toBe('application/json')
  })

  it('reads 401 as nobody signed in and any other failure as an error', async () => {
    expect(await readBridgeSignIn('/b', recordingFetch(() => new Response('', { status: 401 })).fetchImpl)).toBeNull()
    expect(await readBridgeSignIn('/b', recordingFetch(() => new Response(JSON.stringify(signIn))).fetchImpl)).toEqual(signIn)
    await expect(readBridgeSignIn('/b', recordingFetch(() => new Response('bad gateway', { status: 502 })).fetchImpl))
      .rejects.toThrow('502: bad gateway')
  })

  it('shows the bridge\'s refusal message with its code', async () => {
    const refusal = JSON.stringify({ error: { code: 'demo_login_disabled', message: 'demo login is switched off on this server' } })
    expect(bridgeRefusalText(404, refusal)).toBe('demo login is switched off on this server (404 demo_login_disabled)')
    await expect(signInWithDemoLogin('/b', 'principal_000001', recordingFetch(() => new Response(refusal, { status: 404 })).fetchImpl))
      .rejects.toThrow('demo login is switched off on this server (404 demo_login_disabled)')
  })

  it('posts the principal id to demo login', async () => {
    const { calls, fetchImpl } = recordingFetch(() => new Response(JSON.stringify(signIn)))
    expect(await signInWithDemoLogin('/b', 'principal_000001', fetchImpl)).toEqual(signIn)
    expect(calls[0].url).toBe('/b/auth/demo-login')
    expect(calls[0].init.body).toBe('{"principal_id":"principal_000001"}')
  })

  it('logs out with the CSRF token and throws when the server refuses', async () => {
    const ok = recordingFetch(() => new Response(null, { status: 204 }))
    await logOutOfBridge('/b', 'csrf-made-up', ok.fetchImpl)
    expect(new Headers(ok.calls[0].init.headers).get('X-CSRF-Token')).toBe('csrf-made-up')
    const refused = recordingFetch(() => new Response(JSON.stringify({ error: { code: 'csrf_token_mismatch', message: 'wrong token' } }), { status: 403 }))
    await expect(logOutOfBridge('/b', 'stale', refused.fetchImpl)).rejects.toThrow('Could not log out: wrong token (403 csrf_token_mismatch)')
  })
})
