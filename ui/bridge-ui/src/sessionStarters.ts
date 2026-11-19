import type { SessionPrincipal } from '@kayushkin/chat-core'

/** Who started a session, as the sidebar draws it. */
export interface SessionStarter {
  /** principal-store's id, or '' for a session started as no person. */
  principalId: string
  /** One or two letters, or a gear for no person. */
  initials: string
  /** The full words, for the tooltip. */
  title: string
}

const NO_PERSON_LABEL = 'no person'

/** The chip label for a starter: their name, else their id when principal-store
 *  holds no name, else "no person" for sessions started by a script, the
 *  scheduler or an agent. */
export function starterChipLabel(principal: Pick<SessionPrincipal, 'principalId' | 'displayName'>): string {
  if (principal.principalId === '') return NO_PERSON_LABEL
  return principal.displayName || principal.principalId
}

/** Initials of a name: the first letter of its first two words. */
export function initialsOf(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean)
  return words.slice(0, 2).map((word) => [...word][0]!.toUpperCase()).join('')
}

export function sessionStarterOf(principal: Pick<SessionPrincipal, 'principalId' | 'displayName'>): SessionStarter {
  if (principal.principalId === '') {
    return { principalId: '', initials: '⚙', title: 'Started by no person: a script, the scheduler or an agent' }
  }
  const label = starterChipLabel(principal)
  return { principalId: principal.principalId, initials: initialsOf(principal.displayName) || '?', title: `Started by ${label}` }
}
