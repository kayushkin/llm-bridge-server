import { createContext, useContext } from 'react'
import type { ConnectedServerSessionRef } from '../../serverListings'

// Which server a reference chip's id belongs to.
//
// Ids are per server: two bridge servers each number their sessions,
// principals and records on their own, so `principal_000001` in a connected
// server's transcript is that server's principal and almost certainly not
// this server's. A chip drawn inside a connected server's thread must
// therefore never ask this server's stores or resolver what an id names —
// an answer would be about a different record, given as if it were the same.
//
// The connected server's thread sets this context. Inside it a session chip
// loads through that server's store (the thread already runs against it), and
// every other kind draws a plain chip that says which server the id is from
// and that this server cannot look it up. Outside it nothing changes.

export interface RefChipServer {
  /** This server's id for the connection the thread reaches the server by. */
  connectionId: string
  /** The connected server's own name, for the chip's words. */
  serverName: string
  /** Open one of that server's sessions where the chat shows it. Absent where
   *  the thread's own `onActivate` already does the right thing. */
  openSession?: (sessionId: string) => void
}

export const RefChipServerContext = createContext<RefChipServer | null>(null)

export function useRefChipServer(): RefChipServer | null {
  return useContext(RefChipServerContext)
}

/** How a chip of `kind` draws under `server`:
 *  - `local`: this server's own chip, as before;
 *  - `remote-session`: a session chip that loads through the connected server;
 *  - `remote-lookup`: a record id, looked up by the connected server's own
 *    reference-store (its gateway, through the connection), under its access;
 *  - `remote-neutral`: the id, said to be from that server, looked up nowhere:
 *    a file or a button, which belong to that server's session. */
export type RefChipRoute = 'local' | 'remote-session' | 'remote-lookup' | 'remote-neutral'

/** The kinds whose id names a record a reference-store can look up. */
const RECORD_KINDS: ReadonlySet<string> = new Set(['uuid', 'registered', 'note', 'todo'])

export function refChipRoute(kind: string, server: RefChipServer | null): RefChipRoute {
  if (!server) return 'local'
  if (kind === 'session') return 'remote-session'
  return RECORD_KINDS.has(kind) ? 'remote-lookup' : 'remote-neutral'
}

/** Opens a connected server's session in the chat, for a chip in one of this
 *  server's own threads whose session id turned out to live on a connected
 *  server. Provided by the chat page; null elsewhere. */
export const OpenConnectedServerSessionContext = createContext<((ref: ConnectedServerSessionRef) => void) | null>(null)

export function useOpenConnectedServerSession(): ((ref: ConnectedServerSessionRef) => void) | null {
  return useContext(OpenConnectedServerSessionContext)
}

/** Whether a session chip's load error says this server has no such session,
 *  as opposed to a failure to ask. chat-core's ApiClient words a refusal as
 *  `GET <path> failed: <status> …`. */
export function sessionLookupWasNotFound(error: string | null): boolean {
  return error !== null && /\bfailed: 404\b/.test(error)
}
