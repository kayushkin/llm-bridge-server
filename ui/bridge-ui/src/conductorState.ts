// What the chat's conductor — the orchestrator, drawn at the foot of the sidebar — is
// doing, decided from what the orchestrator, its home session and the signals say. Pure,
// so the order of the rules is tested rather than read out of a component.
//
// The orchestrator is one long-lived llm-bridge session (the "home session", whose id
// orchestratord hands out at `GET /home-session`). Its job is to keep the work moving, not
// to do it, so its usual state is `conducting`. It is the one that raises what needs a
// person: the open questions and permission requests every session files as signals.

export type ConductorState = 'offline' | 'alert' | 'busy' | 'trouble' | 'resting' | 'conducting'

/** Home session states that mean it is working on a turn right now. */
const HOME_SESSION_BUSY_STATES: ReadonlySet<string> = new Set(['running', 'tool_running', 'starting'])
/** Home session states that mean its last turn went wrong: ended in error, or its
 *  process went away without a clean exit. */
const HOME_SESSION_TROUBLE_STATES: ReadonlySet<string> = new Set(['error', 'disconnected'])

export interface ConductorFacts {
  /** The orchestrator answered its last call. False when it did not; null before the first. */
  reachable: boolean | null
  /** Autonomous mode (`/config`'s `enabled`): off, the orchestrator only answers when asked. */
  enabled: boolean | null
  /** The home session's state as llm-bridge reports it, or null while this client
   *  does not know the session. */
  homeSessionState: string | null
  /** Open signals the operator is to answer, across every session — the sidebar's
   *  "Needs you". Background agents' signals are not counted. */
  needsYou: number
  /** Of those, the ones raised since you last opened them from the conductor. Only these
   *  make it raise the alarm: a question weeks old is waiting, not urgent. */
  newNeedsYou: number
}

/** In priority order: unreachable first, since nothing else it says can be trusted then;
 *  then anything waiting on you, because that is the reason to look; then busy, because
 *  a turn in flight hides how the last one ended. */
export function conductorState(facts: ConductorFacts): ConductorState {
  if (facts.reachable === false) return 'offline'
  if (facts.newNeedsYou > 0) return 'alert'
  if (facts.homeSessionState !== null && HOME_SESSION_BUSY_STATES.has(facts.homeSessionState)) return 'busy'
  if (facts.homeSessionState !== null && HOME_SESSION_TROUBLE_STATES.has(facts.homeSessionState)) return 'trouble'
  if (facts.enabled === false) return 'resting'
  return 'conducting'
}

/** The words beside the conductor. */
export function conductorStatusText(state: ConductorState, facts: ConductorFacts): string {
  switch (state) {
    case 'offline':
      return 'orchestrator unreachable'
    case 'alert':
      return `${facts.newNeedsYou} new — ${facts.newNeedsYou === 1 ? 'needs' : 'need'} you`
    case 'busy':
      return 'busy — working in its session'
    case 'trouble':
      return `its session is in state ${facts.homeSessionState}`
    case 'resting':
      return 'autonomous mode off — answers when asked'
    case 'conducting':
      return 'conducting'
  }
}

/** How many signals were raised after `seenUpTo` (an ISO time; null counts them all). */
export function signalsRaisedSince(createdAts: readonly string[], seenUpTo: string | null): number {
  if (seenUpTo === null) return createdAts.length
  const seen = Date.parse(seenUpTo)
  return createdAts.filter((createdAt) => Date.parse(createdAt) > seen).length
}

/** The newest of the signals' raise times — what "seen up to" becomes once you open
 *  them. Null when there are none. */
export function newestSignalTime(createdAts: readonly string[]): string | null {
  let newest: string | null = null
  for (const createdAt of createdAts) if (newest === null || Date.parse(createdAt) > Date.parse(newest)) newest = createdAt
  return newest
}
