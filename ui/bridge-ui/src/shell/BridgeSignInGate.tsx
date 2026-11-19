import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import {
  bridgeSignInPaths,
  fetchWithBridgeCsrfToken,
  logOutOfBridge,
  readBridgeSignIn,
  signInWithDemoLogin,
  type BridgeSignIn,
} from '../bridgeSignIn'
import { SignInCard, signInCardClassNames } from './SignInCard'

export interface SignedInToBridge {
  signIn: BridgeSignIn
  /** Sends the cookie, and the CSRF token on every request that changes
   *  something: the `fetch` to hand `<Bridge>`. */
  fetch: typeof fetch
  logOut: () => void
}

export interface BridgeSignInGateProps {
  /** llm-bridge-server's base path, the same one `<Bridge basePath>` takes. */
  basePath: string
  /** The name on the sign-in card. */
  title: string
  children: (signedIn: SignedInToBridge) => ReactNode
}

/** Signs a person in to llm-bridge-server itself, for a host with no sign-in
 *  of its own, then draws `children` with a fetch that carries the session.
 *  It offers both of the server's ways in — Entra ID and demo login — because
 *  the server says which is switched on only by refusing the other, and that
 *  refusal is shown as it came. */
export function BridgeSignInGate({ basePath, title, children }: BridgeSignInGateProps) {
  // undefined while checking; null when nobody is signed in.
  const [signIn, setSignIn] = useState<BridgeSignIn | null | undefined>(undefined)
  const [error, setError] = useState('')
  const [principalId, setPrincipalId] = useState('')
  const [busy, setBusy] = useState(false)

  const check = useCallback(() => {
    readBridgeSignIn(basePath).then(setSignIn).catch(err => setError(err instanceof Error ? err.message : String(err)))
  }, [basePath])
  useEffect(check, [check])

  const signedIn = useMemo<SignedInToBridge | null>(() => {
    if (!signIn) return null
    return {
      signIn,
      fetch: fetchWithBridgeCsrfToken(signIn.csrf_token),
      logOut: () => {
        logOutOfBridge(basePath, signIn.csrf_token)
          .then(() => setSignIn(null))
          .catch(err => setError(err instanceof Error ? err.message : String(err)))
      },
    }
  }, [basePath, signIn])

  if (signedIn) return <>{children(signedIn)}</>
  if (signIn === undefined && !error) return null // checking

  const submitDemoLogin = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!principalId.trim()) {
      setError('A principal id is required, such as principal_000001.')
      return
    }
    setBusy(true)
    setError('')
    try {
      setSignIn(await signInWithDemoLogin(basePath, principalId.trim()))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <SignInCard title={title} prompt="Sign in to this bridge server" error={error}>
      <a className={signInCardClassNames.link} href={bridgeSignInPaths(basePath).entraStart(window.location.pathname + window.location.search)}>
        Sign in with Microsoft
      </a>
      <p className={signInCardClassNames.divider}>or, where demo login is on</p>
      <form onSubmit={submitDemoLogin} style={{ display: 'contents' }}>
        <input
          className={signInCardClassNames.input}
          value={principalId}
          onChange={event => setPrincipalId(event.target.value)}
          placeholder="principal_000001"
          aria-label="Principal id"
        />
        <button type="submit" className={signInCardClassNames.submit} disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in by principal id'}
        </button>
      </form>
    </SignInCard>
  )
}
