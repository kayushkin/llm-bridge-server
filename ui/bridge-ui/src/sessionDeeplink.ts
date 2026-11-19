// Two-way reconciliation between `?session=<bridge_id>` and the focused
// workspace's session.
//
// The URL used to be read once and then wiped, so the address bar never held
// a link that reopens the session: you could not bookmark one, share one, or
// reload into one — while the library itself emits `/?session=<id>` links
// (BridgeKanban, BridgeOrchestrator and RefChip's chatHref, every target taken
// from `routes` — the `hrefFor` this line used to name went with fea0521's
// three hardcoded paths). Now the param names whatever
// session the focused pane holds, and an inbound param opens that session.
//
// Both directions go through this module because the two effects that drive
// them can otherwise fight: the read applies a session to React state, and the
// write sees the *previous* focus in that same commit and would push the old
// id straight back into the URL, which the read then treats as a fresh
// deeplink. `awaiting` is what stops that — after a read, writes are held
// until focus actually lands on the id that was read.
//
// The caller keeps one state value (a ref) and passes it through both calls.

export type SessionDeeplinkState = {
  // The session id the URL is understood to name — whether we read it from
  // there or put it there.
  applied: string | null
  // A deeplink handed to the app whose focus change has not landed yet.
  awaiting: string | null
}

export const initialSessionDeeplinkState: SessionDeeplinkState = { applied: null, awaiting: null }

// Inbound half: given the current `?session=` value, decide whether it names a
// session the app should open. Returns `open: null` for an absent param, and
// for one we ourselves wrote (so our own URL updates never re-trigger a
// select). Re-fires for a genuinely new value, which is what makes an in-app
// `/?session=<id>` link work more than once per page load.
export function readSessionDeeplink(
  param: string | null,
  state: SessionDeeplinkState,
): { open: string | null; state: SessionDeeplinkState } {
  if (!param || param === state.applied) return { open: null, state }
  return { open: param, state: { applied: param, awaiting: param } }
}

// Outbound half: given the session the focused pane now holds, decide what the
// URL should say. `write: false` means leave the address bar alone — either it
// already agrees, or a deeplink is still in flight and the focus we can see is
// the stale pre-deeplink one.
//
// A focused pane with no session (a pending "new chat", or no pane at all)
// clears the param rather than leaving a stale id behind.
export function writeSessionParam(
  focusedSessionId: string | null,
  state: SessionDeeplinkState,
): { write: boolean; value: string | null; state: SessionDeeplinkState } {
  if (state.awaiting !== null) {
    if (focusedSessionId !== state.awaiting) return { write: false, value: null, state }
    // The deeplink landed; the URL already names it.
    return { write: false, value: null, state: { applied: state.applied, awaiting: null } }
  }
  if (focusedSessionId === state.applied) return { write: false, value: null, state }
  return { write: true, value: focusedSessionId, state: { applied: focusedSessionId, awaiting: null } }
}
