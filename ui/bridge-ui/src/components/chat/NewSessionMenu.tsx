import { useCallback, useEffect, useMemo, useRef, type KeyboardEvent, type ReactNode } from 'react'
import { useSessionActions } from '@kayushkin/chat-core'
import { useBridgeHarnesses } from '../../useBridgeHarnesses'
import { useBridgeInstances } from '../../useBridgeInstances'
import { useBridgeMachines } from '../../useBridgeMachines'
import {
  NEW_SESSION_MENU_TOGGLE_ATTRIBUTE,
  newSessionHarnessGroups,
  newSessionMenuServers,
  nextMenuItemIndex,
  type NewSessionMenuScope,
  type NewSessionMenuServer,
} from '../../newSessionMenu'
import { OnConnectedServer, useOptionalConnectedServers } from './ConnectedServers'
import { useNewSessionTarget } from './useNewSessionTarget'

interface NewSessionMenuProps {
  /** Which servers to list: all of them, or the one whose folder's + opened it. */
  scope?: NewSessionMenuScope
  /** A pick on THIS server, with the picked instance and its harness. The
   *  caller merges that harness's saved defaults and starts it. */
  onPick: (opts: { instanceId: string; harness: string }) => void
  /** A session was just started on a connected server (its store now holds the
   *  pending session); show it where the thread is. */
  onStartedOnConnectedServer?: (connectionId: string) => void
  onClose: () => void
}

const MENU_ITEM_SELECTOR = 'button[role="menuitem"]:not(:disabled)'

/** The New session menu: a heading per server — this one first, then each
 *  server it is connected to — and under each that server's own enabled
 *  instances, grouped by harness. A connected server's instances, machines and
 *  harness registry are read under its own store and config
 *  (`OnConnectedServer`), through this server's proxy for the connection, never
 *  from this host's. A server that cannot be read stays listed, greyed, with
 *  the reason, and offers nothing to pick.
 *
 *  Availability is per harness, not per instance: the server reports it on
 *  `HarnessInfo.available`, and every instance of a harness whose process is
 *  gone is equally unreachable. It gates the dot's color, the button's
 *  `disabled`, its title and the click itself together.
 *
 *  Arrow keys move between the items, Home and End jump, Enter picks (the
 *  focused button's own click), Escape closes. Headings are not focusable. */
export default function NewSessionMenu({ scope = { kind: 'all' }, onPick, onStartedOnConnectedServer, onClose }: NewSessionMenuProps) {
  const connected = useOptionalConnectedServers()
  const menuRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const target = e.target as Element | null
      if (target?.closest?.(`[${NEW_SESSION_MENU_TOGGLE_ATTRIBUTE}]`)) return
      if (menuRef.current && !menuRef.current.contains(target as Node)) onClose()
    }
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [onClose])

  // Focus the menu itself when it opens, so the arrow keys reach it before any
  // server's instances have loaded.
  useEffect(() => {
    menuRef.current?.focus()
  }, [])

  const onKeyDown = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
    const items = Array.from(menuRef.current?.querySelectorAll<HTMLButtonElement>(MENU_ITEM_SELECTOR) ?? [])
    const current = items.findIndex(item => item === document.activeElement)
    const next = nextMenuItemIndex(current, items.length, event.key)
    if (next < 0) return
    event.preventDefault()
    items[next].focus()
  }, [])

  const servers = useMemo(
    () => newSessionMenuServers(connected?.thisServerName ?? null, connected?.servers ?? [], scope),
    [connected?.thisServerName, connected?.servers, scope],
  )

  return (
    <div className="bc-new-session-menu" role="menu" ref={menuRef} tabIndex={-1} onKeyDown={onKeyDown}>
      {servers.length === 0 && <div className="bc-new-session-empty">That server is no longer connected.</div>}
      {servers.map(server => {
        if (server.connectionId === null) {
          return (
            <ServerSection key={server.key} server={server}>
              <NewSessionInstanceGroups onPick={(pick) => { onPick(pick); onClose() }} />
            </ServerSection>
          )
        }
        const connectionId = server.connectionId
        const blocked = server.blockedReason
          ?? (connected?.storeOf(connectionId) ? null : `Connecting to ${server.label}…`)
        if (blocked) {
          return (
            <ServerSection key={server.key} server={server} blockedReason={blocked} />
          )
        }
        return (
          <ServerSection key={server.key} server={server}>
            <OnConnectedServer connectionId={connectionId}>
              <ConnectedServerInstanceGroups
                onStarted={() => { onStartedOnConnectedServer?.(connectionId); onClose() }}
              />
            </OnConnectedServer>
          </ServerSection>
        )
      })}
    </div>
  )
}

/** One server's heading and what is under it. */
function ServerSection({ server, blockedReason, children }: {
  server: NewSessionMenuServer
  blockedReason?: string | null
  children?: ReactNode
}) {
  const remote = server.connectionId !== null
  return (
    <div
      className={`bc-new-session-server${blockedReason ? ' bc-new-session-server-blocked' : ''}`}
      data-server={server.key}
    >
      <div className="bc-new-session-server-label" title={blockedReason ?? undefined}>
        {remote ? '🖥 ' : ''}{server.label}
      </div>
      {blockedReason ? <div className="bc-new-session-server-why">{blockedReason}</div> : children}
    </div>
  )
}

/** A connected server's instances. Runs under that server's store and config,
 *  so the instances, machines, harness registry and saved defaults it reads are
 *  that server's, and the session it starts is started there. */
function ConnectedServerInstanceGroups({ onStarted }: { onStarted: () => void }) {
  const target = useNewSessionTarget()
  const { newSession } = useSessionActions()
  return (
    <NewSessionInstanceGroups
      onPick={({ instanceId, harness }) => {
        target.remember(instanceId)
        newSession(target.optsFor(instanceId, harness))
        onStarted()
      }}
    />
  )
}

/** The enabled instances of whichever server's config this is rendered
 *  under, grouped by harness. */
export function NewSessionInstanceGroups({ onPick }: { onPick: (opts: { instanceId: string; harness: string }) => void }) {
  const { instances, loading, error } = useBridgeInstances()
  const { machineMap } = useBridgeMachines()
  const { harnessMap, error: harnessError } = useBridgeHarnesses()
  const groups = useMemo(() => newSessionHarnessGroups(instances, harnessMap), [instances, harnessMap])

  if (error) return <div className="bc-new-session-empty">Could not read its instances: {error}</div>
  if (groups.length === 0) {
    return <div className="bc-new-session-empty">{loading ? 'Reading its instances…' : 'No instances configured.'}</div>
  }
  return (
    <>
      {harnessError && <div className="bc-new-session-empty">Could not read its harnesses, so none is shown as available: {harnessError}</div>}
      {groups.map(group => (
        <div key={group.harness} className="bc-new-session-group">
          <div className="bc-new-session-group-label">
            <span>{group.harness}</span>
          </div>
          {group.instances.map(instance => {
            const machine = instance.machine ?? machineMap.get(instance.machine_id)
            return (
              <button
                key={instance.id}
                type="button"
                className="bc-new-session-item"
                role="menuitem"
                disabled={!group.available}
                title={group.available ? `Create new session in ${instance.name}` : `${group.harness} harness unavailable`}
                onClick={() => {
                  if (!group.available) return
                  onPick({ instanceId: instance.id, harness: instance.harness_type })
                }}
              >
                <span className={`bc-new-session-avail ${group.available ? 'bc-new-session-avail-on' : 'bc-new-session-avail-off'}`} />
                <span className="bc-new-session-item-name">{instance.name}</span>
                <span className="bc-new-session-item-meta">
                  {machine?.emoji ? `${machine.emoji} ` : ''}
                  {machine?.name || '—'}
                </span>
              </button>
            )
          })}
        </div>
      ))}
    </>
  )
}
