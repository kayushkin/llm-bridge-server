import { useMemo } from 'react'
import { ThreadPane } from './BridgeChat'
import { RefChipServerContext, type RefChipServer } from './refChipServer'
import { useNewSessionTarget } from './useNewSessionTarget'

/** Why a connected server's session draws no terminal. */
export const CONNECTED_SERVER_TERMINAL_UNAVAILABLE =
  "not available from here: the terminal's connection does not cross to a connected server. Open the session on that server to use it."

/** A connected server's session in the chat's own thread view: the same
 *  header, controls, panes and composer as a session on this server.
 *
 *  Renders inside `OnConnectedServer` (or the Peers page's own provider), so
 *  every hook below reads the connected server's chat-core store and its
 *  `BridgeConfig`, whose paths `CONNECTED_SERVER_PATH_SOURCES` decides: the
 *  other server's own llm-bridge-server routes through the connection's
 *  proxy, and nothing of this host's. The new-session target is that server's
 *  too — its instances and its `bridge-prefs`. Reference chips in the thread
 *  belong to that server. */
export function ConnectedServerThread({
  connectionId,
  serverName,
  openSession,
}: {
  connectionId: string
  serverName: string
  /** Opens another of that server's sessions where the chat shows it, for a
   *  session chip's ↗. */
  openSession: (sessionId: string) => void
}) {
  const newTarget = useNewSessionTarget()
  const chipServer = useMemo<RefChipServer>(
    () => ({ connectionId, serverName, openSession }),
    [connectionId, serverName, openSession],
  )
  return (
    <RefChipServerContext.Provider value={chipServer}>
      <ThreadPane newTarget={newTarget} terminalUnavailableBecause={CONNECTED_SERVER_TERMINAL_UNAVAILABLE} />
    </RefChipServerContext.Provider>
  )
}
