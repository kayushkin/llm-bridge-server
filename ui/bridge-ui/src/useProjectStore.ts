import { useCallback, useSyncExternalStore } from 'react'
import type { Link, Project } from '@kayushkin/project-store-types'
import { useBridgeConfig } from './context'
import type { FetchFn } from './types'
import {
  fileSessionBody,
  projectLinkPath,
  projectLinksPath,
  projectsPath,
  sessionLinksPath,
  sessionMovePlan,
  type LinkListAnswer,
  type ProjectListAnswer,
} from './projects'

// The projects and every filed session, read once and shared by the sidebar's
// grouping, the session header's filing control and the projects view. Each
// mounts its own reader, and a request per reader would be three of the same
// read; so, like `useDevServerPreviews`, there is one poll per host however
// many components read it. A filing re-reads at once, so the sidebar moves the
// session as soon as project-store has it.

const POLL_INTERVAL_MS = 60_000

/** Who a filing made here names as its author, in project-store's
 *  `created_by`. The browser's person is dash's login, which this library
 *  cannot see; the surface is what it can say truthfully. */
export const FILED_BY = 'bridge-ui session header'

export interface ProjectStoreState {
  projects: Project[]
  sessionLinks: Link[]
  /** The store's refusal verbatim, or the network failure. */
  error: string | null
  /** False until the first answer, so a reader can tell "none" from "not yet". */
  loaded: boolean
}

const NOT_LOADED: ProjectStoreState = { projects: [], sessionLinks: [], error: null, loaded: false }

async function readJSON<T>(response: Response, what: string): Promise<T> {
  if (!response.ok) {
    const body = (await response.text()).trim()
    throw new Error(`${what}: ${response.status} ${response.statusText}${body ? ` — ${body}` : ''}`)
  }
  return (await response.json()) as T
}

class ProjectStoreReader {
  private state: ProjectStoreState = NOT_LOADED
  private readonly listeners = new Set<() => void>()
  private timer: ReturnType<typeof setInterval> | null = null
  private inFlight: Promise<void> | null = null

  private readonly fetchFn: FetchFn
  private readonly base: string

  constructor(fetchFn: FetchFn, base: string) {
    this.fetchFn = fetchFn
    this.base = base
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    if (this.listeners.size === 1) {
      void this.refresh()
      this.timer = setInterval(() => {
        if (typeof document !== 'undefined' && document.hidden) return
        void this.refresh()
      }, POLL_INTERVAL_MS)
    }
    return () => {
      this.listeners.delete(listener)
      if (this.listeners.size === 0 && this.timer) {
        clearInterval(this.timer)
        this.timer = null
      }
    }
  }

  snapshot = (): ProjectStoreState => this.state

  refresh = (): Promise<void> => {
    if (this.inFlight) return this.inFlight
    this.inFlight = (async () => {
      try {
        const [projects, links] = await Promise.all([
          this.fetchFn(projectsPath(this.base)).then((r) => readJSON<ProjectListAnswer>(r, 'project-store projects')),
          this.fetchFn(sessionLinksPath(this.base)).then((r) => readJSON<LinkListAnswer>(r, 'project-store filed sessions')),
        ])
        this.state = { projects: projects.projects, sessionLinks: links.links, error: null, loaded: true }
      } catch (error) {
        this.state = { ...this.state, error: error instanceof Error ? error.message : String(error), loaded: true }
      } finally {
        this.inFlight = null
      }
      for (const listener of this.listeners) listener()
    })()
    return this.inFlight
  }

  /** File `sessionId` under `targetProjectId`, taking it out of every other
   *  project, or out of all of them when the target is null. The new link is
   *  written first; a refusal is thrown verbatim and nothing after it runs. */
  moveSession = async (sessionId: string, targetProjectId: string | null): Promise<void> => {
    const current = this.state.sessionLinks.filter((l) => l.entity_ref === sessionId)
    const plan = sessionMovePlan(current, targetProjectId)
    try {
      if (plan.fileUnder) {
        const response = await this.fetchFn(projectLinksPath(this.base, plan.fileUnder), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: fileSessionBody(sessionId, FILED_BY),
        })
        await readJSON<Link>(response, 'project-store refused the filing')
      }
      for (const link of plan.unfile) {
        const response = await this.fetchFn(projectLinkPath(this.base, link.project_id, link.id), { method: 'DELETE' })
        if (!response.ok) {
          const body = (await response.text()).trim()
          throw new Error(`project-store refused to unfile from ${link.project_id}: ${response.status}${body ? ` — ${body}` : ''}`)
        }
      }
    } finally {
      await this.refresh()
    }
  }
}

const readers = new WeakMap<FetchFn, Map<string, ProjectStoreReader>>()

function readerFor(fetchFn: FetchFn, base: string): ProjectStoreReader {
  let byBase = readers.get(fetchFn)
  if (!byBase) {
    byBase = new Map()
    readers.set(fetchFn, byBase)
  }
  let reader = byBase.get(base)
  if (!reader) {
    reader = new ProjectStoreReader(fetchFn, base)
    byBase.set(base, reader)
  }
  return reader
}

const noSubscribe = (): (() => void) => () => {}
const notLoaded = (): ProjectStoreState => NOT_LOADED
const nothingToRefresh = (): Promise<void> => Promise.resolve()
const nothingToMove = (): Promise<void> => Promise.reject(new Error('this host does not proxy project-store'))

/** The projects and filed sessions, shared; `enabled` is false when the host
 *  proxies no project-store, and then nothing is fetched. */
export function useProjectStore(): ProjectStoreState & {
  enabled: boolean
  refresh: () => Promise<void>
  moveSession: (sessionId: string, targetProjectId: string | null) => Promise<void>
} {
  const { fetch: fetchFn, projectStoreBasePath } = useBridgeConfig()
  const reader = projectStoreBasePath ? readerFor(fetchFn, projectStoreBasePath) : null
  const state = useSyncExternalStore(reader?.subscribe ?? noSubscribe, reader?.snapshot ?? notLoaded, notLoaded)
  const refresh = useCallback(() => (reader ? reader.refresh() : nothingToRefresh()), [reader])
  const moveSession = useCallback(
    (sessionId: string, targetProjectId: string | null) =>
      reader ? reader.moveSession(sessionId, targetProjectId) : nothingToMove(),
    [reader],
  )
  return { ...state, enabled: reader !== null, refresh, moveSession }
}
