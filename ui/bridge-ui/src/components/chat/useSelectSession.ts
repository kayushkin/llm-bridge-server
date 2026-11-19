import { useCallback } from 'react'
import { useActiveSession, useSessionList } from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import { useBridgePrefs } from '../../useBridgePrefs'

/** Open a session AND record it as its instance's last session.
 *
 *  chat already READS `last_session` — `useNewSessionTarget`'s ladder step 2 picks
 *  among the enabled instances of the recorded last harness by preferring one that has
 *  actually been used, and "used" means it has an entry in this record. Until now
 *  chat never wrote it, so that tie-break could only ever be fed by the original chat
 *  at `/`. A user who works in chat alone recorded nothing, and step 2 fell through to
 *  the first enabled instance of the harness — an arbitrary pick standing in for a
 *  preference the page had been told and threw away.
 *
 *  The record is the same server-side one the original chat reads and writes
 *  (`GET/PUT {basePath}/bridge-prefs`, one store per endpoint), so a session opened on
 *  either surface now counts for both.
 *
 *  ## Why every deliberate open, and not just the nav arrows
 *
 *  The todo this comes from (`41f01ca1`) names the arrows, because that is the feature
 *  it is about. Measured, chat wrote the field nowhere — not from the arrows and not
 *  from a sidebar click. Recording only the arrows would make the record depend on which
 *  control the user reached for, which is not a rule anyone chose. bridge-ui writes on
 *  both (both `setLastSession` calls in `Workspace.tsx` for the arrows, `BridgeChat.tsx`
 *  for a click).
 *
 *  ## Where the line is drawn, and why it is the list
 *
 *  Three callers. Two are the sidebar's rows and the header's nav arrows, and they are
 *  the same surface: the arrows walk the flattening of the very rows the sidebar renders
 *  (`SessionHeader.tsx`'s `orderedIds`). That is also the domain this hook can answer
 *  "which instance?" over.
 *
 *  The third is a subagent chip in the Timeline pane (`Timeline.tsx`), which arrived
 *  later and is a deliberate exception to the line drawn below: a subagent session is
 *  named by a chip rather than picked off the visible list, so the lookup is exactly as
 *  unreliable there as it is for the `[session:…]` chips that are excluded. It is here
 *  because opening a subagent IS the user deliberately switching panes, and Timeline
 *  holds the hook through a ref precisely so the list churn does not re-render the pane.
 *  ⚠️ If the lookup misses, the write is the same silent no-op described below.
 *
 *  Two other places open a session and deliberately do NOT come through here:
 *
 *   - the `?session=` deeplink (`Chat.tsx`). It runs on mount, before the list has
 *     loaded, so the lookup below would miss every time and the write would be a silent
 *     no-op that reads like a rule. It is also not the user picking: those links are
 *     emitted by kanban cards, the Orchestrator page and chat-core's reference chips
 *     (`RefChip`'s `chatHref`), so recording them would let a machine-generated link
 *     decide where the user's next "+ New" lands.
 *
 *     ⚠️ This used to add "(bridge-ui does route its deeplink through the writing path —
 *     noting the divergence rather than copying it)". Measured 2026-08-17 against the
 *     live `/`, that divergence does not exist. bridge-ui's deeplink effect
 *     (`BridgeChat.tsx:302`) calls `handleSelectSession`, which writes only
 *     `if (session?.instance_id)` — and on a cold load `bridge.sessions` is still empty,
 *     so it takes the same silent no-op described above. Counting `PUT /bridge-prefs` in
 *     the browser:
 *
 *         cold  `/?session=<id>`                              -> 0 writes
 *         warm  (list loaded, then the param changes)         -> 2 writes
 *         click a row on a loaded list (the control)          -> 2 writes
 *
 *     A deeplink is only ever entered cold — kanban cards, the Orchestrator page and
 *     the reference chips all emit a fresh navigation — so bridge-ui's deeplink writes nothing on
 *     the only path it is used on. The warm case is a param change on a page that is
 *     already open, which is a click by another name.
 *
 *     So the two surfaces already agree, and the reasons above are why the agreement is
 *     the right one rather than an accident to be corrected. Do not "align chat with
 *     bridge-ui" by adding a write here: that would be aligning with a behavior
 *     bridge-ui does not have.
 *   - a `[session:…]` reference chip in a transcript (`TurnList.tsx`). The session it
 *     names need not be in the visible list at all, so the same lookup is unreliable
 *     there in a way it is not here.
 *
 *  ## What this deliberately does NOT write
 *
 *  ⛔ `last_instance_id`, which bridge-ui writes alongside on a click. Leaving it is the
 *  decision-free line and copying it would not be:
 *
 *   - It is step 1 of the same ladder, so writing it moves where a bare "+ New" lands.
 *     chat has its own narrower rule for that — `newTarget.remember` fires only on a
 *     deliberate instance pick in the new-session menu (`Sidebar.tsx`) — and widening it
 *     to "whatever session you last looked at" is a product change, not a port.
 *   - bridge-ui's own arrows omit it, and the reason does not carry over: its `navOrder`
 *     is filtered to the active session's instance (`navOrder` in `Workspace.tsx`), so the
 *     arrows cannot leave the instance and writing it there would be a no-op. chat's
 *     arrows walk the flattened sidebar and DO cross instances. Copying the omission
 *     blind would be fidelity to a line whose reason is absent here — and whether those
 *     arrows should cross instances at all is the open half of `41f01ca1`.
 *
 *  So: `last_session` only. It is purely additive — nothing here wrote it before — and it
 *  cannot change an existing "+ New" target, only feed the tie-break the field is for. */
export function useSelectSession(): (sessionId: string) => void {
  const { select } = useActiveSession()
  const { groups } = useSessionList()
  const { fetch: bridgeFetch, basePath } = useBridgeConfig()
  const { setLastSession } = useBridgePrefs({
    fetch: bridgeFetch,
    endpoint: `${basePath}/bridge-prefs`,
  })

  return useCallback(
    (sessionId: string) => {
      // The open happens first and unconditionally. Remembering is a side effect of the
      // navigation, never a precondition for it — a prefs write that cannot be resolved
      // must not cost the user the click.
      select(sessionId)

      // `groups` is the visible list. Two of the three callers get the id they pass out
      // of it — the sidebar renders these rows and the header's arrows walk this same
      // flattening — so from those two a miss is not reachable. The third is Timeline's
      // subagent chip (Timeline.tsx:53, 59), which passes a session id read off the
      // transcript rather than off this list: a subagent session outside the loaded
      // page, or filtered out by an active chip, is not in `groups` at all. So the miss
      // below is a path the UI really reaches. It is handled rather than asserted,
      // because the honest answer to "which instance is that" for a session this page
      // cannot see is "unknown" — and writing the wrong instance is worse than writing
      // nothing.
      let instanceId = ''
      for (const g of groups) {
        for (const s of g.sessions) {
          if (s.sessionId === sessionId) {
            instanceId = s.instanceId
            break
          }
        }
        if (instanceId) break
      }
      if (!instanceId) return

      // ⚠️ bridge-ui declares this as `setLastSession(harness, sessionId)` and the name is
      // wrong: every one of its own call sites passes `session.instance_id`, and both
      // ladders that read the record test `instance.id in last_session`. Verified against
      // this host's saved record — the keys are instance ids (`inst-cc-local`, …), never
      // harness names. Pass the instance.
      setLastSession(instanceId, sessionId)
    },
    [select, groups, setLastSession],
  )
}
