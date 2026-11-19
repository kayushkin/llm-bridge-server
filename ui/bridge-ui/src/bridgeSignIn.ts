// Signing in to llm-bridge-server itself, for a host that has no sign-in of
// its own (bridge-ui's standalone app). dash signs people in with its own
// server and never uses this.
//
// The server's routes (llm-bridge-server internal/server/login_sessions.go,
// demo_login.go, entra_sign_in.go): `GET /auth/principal` answers who the
// `llm_bridge_principal_session` cookie names and the session's CSRF token, or
// 401 with no live session; `POST /auth/demo-login {"principal_id"}` signs in
// as a principal-store id when demo login is on (404 `demo_login_disabled`
// otherwise); `GET /auth/entra/start?return_to=` sends the browser to Entra ID;
// `POST /auth/logout` revokes the session. Every request but GET, HEAD and
// OPTIONS made with the cookie must carry the CSRF token in `X-CSRF-Token`.

/** Who the bridge says is signed in: `GET /auth/principal`'s answer. */
export interface BridgeSignIn {
  principal_id: string
  expires_at: string
  login_session_id: string
  method: string
  csrf_token: string
}

export function bridgeSignInPaths(basePath: string) {
  return {
    principal: `${basePath}/auth/principal`,
    demoLogin: `${basePath}/auth/demo-login`,
    logout: `${basePath}/auth/logout`,
    entraStart: (returnTo: string) => `${basePath}/auth/entra/start?return_to=${encodeURIComponent(returnTo)}`,
  }
}

const METHODS_WITHOUT_CSRF_TOKEN = new Set(['GET', 'HEAD', 'OPTIONS'])

/** Whether the bridge refuses a request by this method without the CSRF token. */
export function methodNeedsCsrfToken(method: string | undefined): boolean {
  return !METHODS_WITHOUT_CSRF_TOKEN.has((method ?? 'GET').toUpperCase())
}

/** The text a refusal shows: the bridge's `{"error":{"code","message"}}`
 *  message with its code, or the body as it came when it is not that shape. */
export function bridgeRefusalText(status: number, body: string): string {
  try {
    const parsed = JSON.parse(body) as { error?: { code?: string; message?: string } }
    if (parsed.error?.message) {
      return `${parsed.error.message} (${status}${parsed.error.code ? ` ${parsed.error.code}` : ''})`
    }
  } catch {
    // Not JSON: shown as it came, below.
  }
  return `${status}: ${body.trim()}`
}

/** A fetch that sends the cookie and, on every request that changes
 *  something, the session's CSRF token. */
export function fetchWithBridgeCsrfToken(csrfToken: string, fetchImpl: typeof fetch = fetch): typeof fetch {
  return (input, init = {}) => {
    const method = init.method ?? (input instanceof Request ? input.method : 'GET')
    if (!methodNeedsCsrfToken(method)) return fetchImpl(input, { credentials: 'include', ...init })
    const headers = new Headers(init.headers ?? (input instanceof Request ? input.headers : undefined))
    headers.set('X-CSRF-Token', csrfToken)
    return fetchImpl(input, { credentials: 'include', ...init, headers })
  }
}

/** null when no live login session is on the cookie; a throw for any other
 *  failure, since "not signed in" would then be a guess. */
export async function readBridgeSignIn(basePath: string, fetchImpl: typeof fetch = fetch): Promise<BridgeSignIn | null> {
  const res = await fetchImpl(bridgeSignInPaths(basePath).principal, { credentials: 'include' })
  if (res.status === 401) return null
  if (!res.ok) throw new Error(`Could not check the sign-in: ${bridgeRefusalText(res.status, await res.text())}`)
  return (await res.json()) as BridgeSignIn
}

/** Signs in by demo login. The bridge's refusal is thrown as it came. */
export async function signInWithDemoLogin(basePath: string, principalId: string, fetchImpl: typeof fetch = fetch): Promise<BridgeSignIn> {
  const res = await fetchImpl(bridgeSignInPaths(basePath).demoLogin, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ principal_id: principalId }),
  })
  if (!res.ok) throw new Error(bridgeRefusalText(res.status, await res.text()))
  return (await res.json()) as BridgeSignIn
}

/** Revokes the login session on the server. A refusal is thrown, so a page
 *  never says "logged out" while the session lives. */
export async function logOutOfBridge(basePath: string, csrfToken: string, fetchImpl: typeof fetch = fetch): Promise<void> {
  const res = await fetchWithBridgeCsrfToken(csrfToken, fetchImpl)(bridgeSignInPaths(basePath).logout, { method: 'POST' })
  if (!res.ok) throw new Error(`Could not log out: ${bridgeRefusalText(res.status, await res.text())}`)
}
