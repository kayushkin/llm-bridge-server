import { useCallback, useEffect, useState } from 'react'
import { useBridgeConfig } from '../../context'
import type { GitRepo } from '../GitPanel'

/**
 * The repositories a session can see, and which one the Git pane is reading.
 *
 * ## Why the pane does not fetch this itself
 *
 * `GitPanel` takes its repo list and selection as props rather than reading them,
 * and that is deliberate on bridge-ui's side: in the original chat the same
 * selection drives both the pane and the repo dropdown in the session header, so a
 * pane that fetched its own list would fight the dropdown. chat has no such
 * dropdown yet, but the shape is the one the pane exports, and owning the state
 * here is what lets a second reader appear later without moving anything.
 *
 * ## What re-reads it
 *
 * The repo LIST is re-read on a session swap, on every new `refetchSignal`, and on
 * an explicit refresh. The pane re-reads the repo's own status and diffs on the
 * same signal — see `GitPanelProps.refetchSignal`.
 *
 * ⚠️ `refetchSignal` is a value that CHANGES when the tree might have, not a
 * meaning. chat passes the session's state string, so the tree is re-read at
 * every turn boundary: an agent that just finished editing files is exactly when
 * the diff on screen has gone stale.
 */
export interface SessionGitRepos {
  repos: GitRepo[]
  selectedRepo: string
  setSelectedRepo: (path: string) => void
  loading: boolean
  error: string | null
  refresh: () => void
}

export function useGitRepos(
  sessionId: string | null,
  refetchSignal: string,
): SessionGitRepos {
  const { fetch: bridgeFetch, basePath } = useBridgeConfig()

  const [repos, setRepos] = useState<GitRepo[]>([])
  const [selectedRepo, setSelectedRepo] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refreshTick, setRefreshTick] = useState(0)

  const refresh = useCallback(() => setRefreshTick(tick => tick + 1), [])

  useEffect(() => {
    if (!sessionId) {
      // Cleared rather than left standing. A stale repo list under a session that
      // does not own it is worse than an empty pane: the paths look plausible and
      // the diff below them belongs to somebody else's work.
      setRepos([])
      setError(null)
      return
    }
    let canceled = false
    setLoading(true)
    setError(null)
    bridgeFetch(`${basePath}/sessions/${sessionId}/git/repos`)
      .then(async response => {
        if (!response.ok) throw new Error(`${response.status} ${await response.text()}`)
        return (await response.json()) as { repos?: GitRepo[] }
      })
      .then(body => {
        if (!canceled) setRepos(body.repos ?? [])
      })
      .catch((err: unknown) => {
        if (canceled) return
        // Stated, not swallowed. `GitPanel` renders this string, so a session on a
        // machine that has gone away says so instead of showing an empty repo
        // dropdown that looks like a session with no repositories.
        setError(`repos: ${err instanceof Error ? err.message : String(err)}`)
        setRepos([])
      })
      .finally(() => {
        if (!canceled) setLoading(false)
      })
    return () => {
      canceled = true
    }
  }, [sessionId, refetchSignal, refreshTick, bridgeFetch, basePath])

  useEffect(() => {
    if (repos.length === 0) {
      setSelectedRepo('')
      return
    }
    // Re-pick when the current selection is not in the list — a session swap, or a
    // repo that has gone. Falling through to the first entry rather than keeping a
    // path this session cannot read is what stops the pane asking for a repo the
    // server will 404, over and over, on every signal.
    if (!selectedRepo || !repos.some(repo => repo.path === selectedRepo)) {
      setSelectedRepo(repos[0]!.path)
    }
  }, [repos, selectedRepo])

  return { repos, selectedRepo, setSelectedRepo, loading, error, refresh }
}
