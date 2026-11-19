import { useEffect, useMemo, useState } from 'react'
import type {
  MachineStatsHistory,
  MachineStatsOverview,
  PeerConnectionView,
  ServerDescription,
} from '@kayushkin/llm-bridge-types'
import type { BridgeConfig } from './context'
import type { FetchFn } from './types'
import { PEER_CONNECTIONS_PATH, peerConnectionBridgeConfig, peerRouteErrorText } from './peers'
import { THIS_SERVER_PATH } from './serverListings'
import { SharedPoll, failureOfResponse, sharedPoll, useSharedPoll, type LoadResult, type PollSnapshot } from './sharedPoll'
import {
  MACHINE_STATS_HISTORY_REFRESH_MS,
  MACHINE_STATS_PATH,
  MACHINE_STATS_POLL_MS,
  machineStatsHistoryPath,
  machineStatsHistoryQuery,
  machineStatsServers,
  responseMeansNotReported,
  type MachineStatsAnswer,
  type MachineStatsHistoryRange,
  type MachineStatsServer,
} from './machineStats'

/** GET a machine-stats route. A 404 from a server built before the route is
 *  an answer — `not reported` — not a failure; every other refusal is shown
 *  in the server's own words. */
export async function loadMachineStatsAnswer<T>(fetchFn: FetchFn, basePath: string, path: string): Promise<LoadResult<MachineStatsAnswer<T>>> {
  const verb = `GET ${path.split('?')[0]}`
  let res: Response
  try {
    res = await fetchFn(`${basePath}${path}`)
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}`, failure: 'unreachable' }
  }
  if (!res.ok) {
    const failure = failureOfResponse(res)
    const body = await res.clone().text().catch(() => '')
    if (responseMeansNotReported(res.status, res.headers.get('Content-Type') ?? '', body)) {
      return { ok: true, value: { state: 'not reported' } }
    }
    return { ok: false, error: await peerRouteErrorText(res, verb), failure }
  }
  try {
    return { ok: true, value: { state: 'reported', value: (await res.json()) as T } }
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}`, failure: 'refused' }
  }
}

async function loadJSONOrError<T>(fetchFn: FetchFn, url: string, verb: string, emptyOnForbidden: T | null): Promise<LoadResult<T>> {
  let res: Response
  try {
    res = await fetchFn(url)
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}`, failure: 'unreachable' }
  }
  // Only an administrator may list connections; anyone else sees this server
  // alone, which is not an error.
  if (emptyOnForbidden !== null && (res.status === 401 || res.status === 403)) return { ok: true, value: emptyOnForbidden }
  if (!res.ok) return { ok: false, error: await peerRouteErrorText(res, verb), failure: failureOfResponse(res) }
  try {
    return { ok: true, value: (await res.json()) as T }
  } catch (err) {
    return { ok: false, error: `${verb}: ${err instanceof Error ? err.message : String(err)}`, failure: 'refused' }
  }
}

export interface MachineStatsServersValue {
  servers: MachineStatsServer[]
  /** Why this server's description or the connection list could not be read. */
  error: string | null
}

/** This server's own description, read every five minutes: only its name
 *  is shown here, and that changes rarely. */
export function serverDescriptionPoll(fetchFn: FetchFn, basePath: string): SharedPoll<ServerDescription | null> {
  return sharedPoll(fetchFn, `server-description ${basePath}`, () =>
    new SharedPoll<ServerDescription | null>(() => loadJSONOrError<ServerDescription | null>(fetchFn, `${basePath}${THIS_SERVER_PATH}`, `GET ${THIS_SERVER_PATH}`, null), null, 5 * 60_000))
}

/** The connections to other servers, read every 30 seconds: whether each can
 *  be read changes without this page doing anything. */
export function peerConnectionsPoll(fetchFn: FetchFn, basePath: string): SharedPoll<PeerConnectionView[]> {
  return sharedPoll(fetchFn, `peer-connections ${basePath}`, () =>
    new SharedPoll<PeerConnectionView[]>(() => loadJSONOrError<PeerConnectionView[]>(fetchFn, `${basePath}${PEER_CONNECTIONS_PATH}`, `GET ${PEER_CONNECTIONS_PATH}`, []), [], MACHINE_STATS_POLL_MS))
}

/** This server and every connected server, for the Machines page. */
export function useMachineStatsServers(config: BridgeConfig): MachineStatsServersValue {
  const { fetch: fetchFn, basePath } = config
  const description = useSharedPoll(serverDescriptionPoll(fetchFn, basePath))
  const connections = useSharedPoll(peerConnectionsPoll(fetchFn, basePath))
  const servers = useMemo(() => machineStatsServers(description.data, connections.data), [description.data, connections.data])
  return { servers, error: [description.error, connections.error].filter(Boolean).join(' · ') || null }
}

/** The config a server's routes are read under: this one, or the
 *  connection's proxy to a connected server. */
export function machineStatsServerConfig(config: BridgeConfig, server: MachineStatsServer): BridgeConfig {
  return server.connectionId ? peerConnectionBridgeConfig(config, server.connectionId) : config
}

/** One server's machines with their newest readings, read every 30 seconds
 *  by one poll however many components ask. */
export function machineStatsOverviewPoll(fetchFn: FetchFn, basePath: string): SharedPoll<MachineStatsAnswer<MachineStatsOverview[]> | null> {
  return sharedPoll(fetchFn, `machine-stats ${basePath}`, () =>
    new SharedPoll<MachineStatsAnswer<MachineStatsOverview[]> | null>(
      () => loadMachineStatsAnswer<MachineStatsOverview[]>(fetchFn, basePath, MACHINE_STATS_PATH),
      null,
      MACHINE_STATS_POLL_MS,
    ))
}

export function useMachineStatsOverview(serverConfig: BridgeConfig): PollSnapshot<MachineStatsAnswer<MachineStatsOverview[]> | null> {
  return useSharedPoll(machineStatsOverviewPoll(serverConfig.fetch, serverConfig.basePath))
}

export interface MachineStatsHistoryValue {
  answer: MachineStatsAnswer<MachineStatsHistory> | null
  loading: boolean
  error: string | null
}

/** One machine's history over a range ending now, read when the range is
 *  picked and again every minute while it is shown. The last answer stays on
 *  screen while the next one loads. */
export function useMachineStatsHistory(serverConfig: BridgeConfig, machineID: string, range: MachineStatsHistoryRange): MachineStatsHistoryValue {
  const { fetch: fetchFn, basePath } = serverConfig
  const [value, setValue] = useState<MachineStatsHistoryValue>({ answer: null, loading: true, error: null })

  useEffect(() => {
    let canceled = false
    const read = async () => {
      setValue(previous => ({ ...previous, loading: true }))
      const path = machineStatsHistoryPath(machineID, machineStatsHistoryQuery(range, new Date()))
      const result = await loadMachineStatsAnswer<MachineStatsHistory>(fetchFn, basePath, path)
      if (canceled) return
      setValue(previous => result.ok
        ? { answer: result.value, loading: false, error: null }
        : { answer: previous.answer, loading: false, error: result.error })
    }
    void read()
    const timer = setInterval(() => void read(), MACHINE_STATS_HISTORY_REFRESH_MS)
    return () => { canceled = true; clearInterval(timer) }
  }, [fetchFn, basePath, machineID, range])

  return value
}
