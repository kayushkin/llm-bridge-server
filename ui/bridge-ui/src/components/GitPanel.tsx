import { useCallback, useEffect, useMemo, useState } from 'react'
import { useBridgeConfig } from '../context'
// The git pane's own type. It lived in the deleted chat's WorkspaceContext until
// that went; the pane takes its repos as props and always did.
export interface GitRepo {
  path: string
  name: string
}
import { UnifiedDiffView } from './tools/DiffView'

interface GitView {
  repo: string
  branch: string
  status: string
  diff_unstaged: string
  diff_staged: string
  log: string
}

type Section = 'status' | 'diff_unstaged' | 'diff_staged' | 'log'

/** Which sections hold a unified diff. A named predicate rather than an inline
 *  `startsWith('diff')`: `diff_unstaged` and `diff_staged` are the only two, and a
 *  section added later that merely happens to begin with those letters should not
 *  silently start being colorized. */
function isDiffSection(section: Section): boolean {
  return section === 'diff_unstaged' || section === 'diff_staged'
}

const SECTION_LABELS: Record<Section, string> = {
  status: 'Status',
  diff_unstaged: 'Unstaged',
  diff_staged: 'Staged',
  log: 'Log',
}

export interface GitPanelProps {
  sessionId: string
  /** Any value that changes when the working tree might have. The panel
   *  re-reads the repo on every new value and never inspects it.
   *
   *  It was typed `SessionUIState` and named `uiState`, which claimed the panel
   *  cared what the session was doing. It does not — the value appears once, in
   *  a dependency array. The narrow type was also the only thing stopping a host
   *  that tracks session state as a plain string (dash's chat page, on chat-core) from
   *  mounting this pane at all, for a distinction the panel cannot act on. */
  refetchSignal: string
  // The repo list and the selection are the caller's, not this pane's. Inside
  // the chat they belong to Workspace, which shares them with SessionHeader's
  // repo dropdown so both reflect the same selection — and a pane that fetched
  // its own would fight that dropdown. Passing them in is also what lets a host
  // building its own layout mount this pane at all: they used to be read from
  // the workspace context directly, and `useWorkspace` throws outside its
  // provider, so the pane could only ever live inside the whole chat.
  gitRepos: GitRepo[]
  selectedRepo: string
  setSelectedRepo: (path: string) => void
  gitReposLoading: boolean
  gitReposError: string | null
  refreshGitRepos: () => void
  onToggleCollapse: () => void
  style?: React.CSSProperties
  paneKey?: string
}

export function GitPanel({
  sessionId,
  refetchSignal,
  gitRepos: repos,
  selectedRepo,
  setSelectedRepo,
  gitReposLoading: loadingRepos,
  gitReposError: reposError,
  refreshGitRepos,
  onToggleCollapse,
  style,
  paneKey,
}: GitPanelProps) {
  const { fetch: fetchFn, basePath } = useBridgeConfig()

  const [view, setView] = useState<GitView | null>(null)
  const [viewError, setViewError] = useState<string | null>(null)
  const [loadingView, setLoadingView] = useState(false)
  const [openSections, setOpenSections] = useState<Record<Section, boolean>>({
    status: true,
    diff_unstaged: true,
    diff_staged: false,
    log: false,
  })
  // Bumped manually to force a view refetch even when nothing else changed.
  const [refreshTick, setRefreshTick] = useState(0)

  const refresh = useCallback(() => {
    refreshGitRepos()
    setRefreshTick(t => t + 1)
  }, [refreshGitRepos])

  // Fetch the four-pane view for the selected repo. Repos themselves are
  // fetched at workspace level so they stay populated even when this pane
  // is hidden.
  useEffect(() => {
    if (!sessionId || !selectedRepo) {
      setView(null)
      return
    }
    let canceled = false
    setLoadingView(true)
    setViewError(null)
    const url = `${basePath}/sessions/${sessionId}/git?repo=${encodeURIComponent(selectedRepo)}`
    fetchFn(url)
      .then(async r => {
        if (!r.ok) throw new Error(`${r.status} ${await r.text()}`)
        return r.json() as Promise<GitView>
      })
      .then(data => { if (!canceled) setView(data) })
      .catch(err => {
        if (canceled) return
        setViewError(`git: ${err instanceof Error ? err.message : String(err)}`)
        setView(null)
      })
      .finally(() => { if (!canceled) setLoadingView(false) })
    return () => { canceled = true }
  }, [sessionId, selectedRepo, refetchSignal, refreshTick, fetchFn, basePath])

  const error = reposError || viewError

  const toggleSection = useCallback((s: Section) => {
    setOpenSections(prev => ({ ...prev, [s]: !prev[s] }))
  }, [])

  const sectionContent = useMemo<Record<Section, string>>(() => ({
    status: view?.status ?? '',
    diff_unstaged: view?.diff_unstaged ?? '',
    diff_staged: view?.diff_staged ?? '',
    log: view?.log ?? '',
  }), [view])

  return (
    <div className="bc-split-pane bc-split-pane-git" style={style} data-pane={paneKey}>
      <div
        className="bc-split-pane-header bc-header-clickable"
        onClick={onToggleCollapse}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onToggleCollapse() } }}
        role="button"
        tabIndex={0}
        title="Collapse git"
        aria-label="Collapse git"
      >
        <span className="bc-split-pane-title">Git</span>
        {repos.length > 0 && (
          <select
            className="bc-git-repo-select"
            value={selectedRepo}
            onChange={e => setSelectedRepo(e.target.value)}
            onClick={e => e.stopPropagation()}
            onKeyDown={e => e.stopPropagation()}
            title="Switch repository"
          >
            {repos.map(r => (
              <option key={r.path} value={r.path}>{r.name}</option>
            ))}
          </select>
        )}
        <span className="bc-spacer" />
        <button
          className="bc-split-collapse-btn"
          onClick={e => { e.stopPropagation(); refresh() }}
          title="Refresh git data"
          aria-label="Refresh git data"
          disabled={loadingRepos || loadingView}
        >⟳</button>
        <span className="bc-split-collapse-btn" aria-hidden="true">▸</span>
      </div>
      <div className="bc-git-body">
        {error && <div className="bc-git-error">{error}</div>}
        {!error && repos.length === 0 && !loadingRepos && (
          <div className="bc-git-empty">
            No git repositories discovered yet for this session.
          </div>
        )}
        {!error && repos.length > 0 && view && (
          <>
            <div className="bc-git-branch">
              <span className="bc-git-branch-label">on</span>
              <span className="bc-git-branch-name">{view.branch || '(unknown)'}</span>
              <span className="bc-git-repo-path" title={view.repo}>{view.repo}</span>
            </div>
            {(['status', 'diff_unstaged', 'diff_staged', 'log'] as Section[]).map(s => {
              const open = openSections[s]
              const content = sectionContent[s]
              return (
                <div key={s} className="bc-git-section">
                  <button
                    className="bc-git-section-header"
                    onClick={() => toggleSection(s)}
                  >
                    <span className="bc-git-section-chevron">{open ? '▾' : '▸'}</span>
                    <span className="bc-git-section-title">{SECTION_LABELS[s]}</span>
                    {!open && content && (
                      <span className="bc-git-section-hint">{content.split('\n').length} lines</span>
                    )}
                  </button>
                  {open && (
                    // The two DIFF sections are rendered as diffs; `status` and `log`
                    // are not diffs and must not be colored as though they were. A
                    // `git status` line beginning `-` is a deleted file in a porcelain
                    // listing, not a removed line, and `git log` prose starting with a
                    // `+` is just prose — painting either red or green states something
                    // the output does not say.
                    //
                    // ⚠️ `UnifiedDiffView`, NOT `DiffView`. `DiffView` takes a file's
                    // before/after CONTENTS and computes the patch itself; the git
                    // endpoint returns `git diff` output, already unified and often
                    // spanning several files, so there are no two versions to hand it.
                    isDiffSection(s) ? (
                      content ? (
                        <UnifiedDiffView diff={content} />
                      ) : (
                        <pre className="bc-git-section-body">
                          <span className="bc-git-section-empty">(empty)</span>
                        </pre>
                      )
                    ) : (
                      <pre className="bc-git-section-body">
                        {content || <span className="bc-git-section-empty">(empty)</span>}
                      </pre>
                    )
                  )}
                </div>
              )
            })}
          </>
        )}
      </div>
    </div>
  )
}
