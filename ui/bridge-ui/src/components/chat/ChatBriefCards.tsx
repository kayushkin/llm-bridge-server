import { memo, useCallback, useEffect, useState, type ReactNode } from 'react'
import {
  openLooseEnds,
  sessionBriefChanges,
  subscribeToSessionBriefChanges,
  type LooseEnd,
  type SignalRequest,
} from '@kayushkin/chat-core'
import { useBridgeConfig } from '../../context'
import type { ChatBriefCard, OrchestratorRemark } from '../../chatBriefSections'
import { timeAgo } from '../../utils'
import styles from './Chat.module.css'

// One card per chat on the signals page: what the chat is for, what it is doing,
// what it changed, the loose ends it left (things it said that wanted an answer
// and never got one), the orchestrator's remarks on it, and its open questions.

/** What each loose end kind is called on the card. */
const looseEndKindLabel: Record<LooseEnd['kind'], string> = {
  question: 'Unanswered question',
  offer: 'Offer not taken up',
  dropped_problem: 'Problem left behind',
}

export interface OrchestratorRemarksState {
  remarks: OrchestratorRemark[]
  /** Set when orchestratord could not be read; the cards then show no remarks
   *  and the page says why. Null when there is no orchestrator configured. */
  error: string | null
  reload: () => void
  close: (remark: OrchestratorRemark) => Promise<void>
}

/**
 * The orchestrator's open remarks, from orchestratord's GET /remarks through the
 * host's orchestrator path. A host with no orchestrator configured shows none.
 * Re-read whenever a brief changes, since the orchestrator remarks on what the
 * briefs show, and on the page's own Refresh.
 */
export function useOrchestratorRemarks(): OrchestratorRemarksState {
  const { fetch: apiFetch, orchestratorBasePath } = useBridgeConfig()
  const [remarks, setRemarks] = useState<OrchestratorRemark[]>([])
  const [error, setError] = useState<string | null>(null)
  const [reloadToken, setReloadToken] = useState(0)
  const reload = useCallback(() => setReloadToken(token => token + 1), [])
  useEffect(() => subscribeToSessionBriefChanges(reload), [reload])

  useEffect(() => {
    if (!orchestratorBasePath) return undefined
    let live = true
    apiFetch(`${orchestratorBasePath}/remarks`)
      .then(async response => {
        if (!response.ok) throw new Error(`GET /remarks answered ${response.status}`)
        const answer: unknown = await response.json()
        if (!Array.isArray(answer)) throw new Error('GET /remarks did not answer a list')
        if (live) {
          setRemarks(answer as OrchestratorRemark[])
          setError(null)
        }
      })
      .catch((e: unknown) => {
        if (live) setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      live = false
    }
  }, [apiFetch, orchestratorBasePath, reloadToken])

  const close = useCallback(
    async (remark: OrchestratorRemark) => {
      const response = await apiFetch(`${orchestratorBasePath}/remarks/${encodeURIComponent(remark.id)}/close`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ because: 'closed by the person on the signals page' }),
      })
      if (!response.ok) throw new Error(`closing ${remark.id} answered ${response.status}`)
      reload()
    },
    [apiFetch, orchestratorBasePath, reload],
  )

  return { remarks, error, reload, close }
}

export const ChatBriefCardView = memo(function ChatBriefCardView({
  card,
  sessionName,
  onSelectSession,
  renderRequest,
  answerLooseEnd,
  dismissLooseEnd,
  closeRemark,
}: {
  card: ChatBriefCard
  sessionName: string
  onSelectSession: (sessionId: string) => void
  renderRequest: (request: SignalRequest) => ReactNode
  answerLooseEnd: (looseEnd: LooseEnd, text: string) => Promise<void>
  dismissLooseEnd: (looseEnd: LooseEnd) => Promise<void>
  closeRemark: (remark: OrchestratorRemark) => Promise<void>
}) {
  const { brief } = card
  const changes = sessionBriefChanges(brief)
  const looseEnds = openLooseEnds(brief)
  const openSession = useCallback(() => onSelectSession(brief.sessionId), [onSelectSession, brief.sessionId])

  return (
    <article className={styles.chatBriefCard} aria-label={`Brief of ${sessionName}`}>
      <div className={styles.signalsPageMeta}>
        <button type="button" className={styles.inboxSessionButton} title="Open this chat" onClick={openSession}>
          {sessionName}
        </button>
        <span title={brief.updatedAt}>updated {timeAgo(brief.updatedAt)}</span>
        <span>{brief.revisionCount} {brief.revisionCount === 1 ? 'turn' : 'turns'}</span>
      </div>
      <dl className={styles.chatBriefFields}>
        <dt>Goal</dt>
        <dd>{brief.overallGoal}</dd>
        {brief.currentStep !== '' && (
          <>
            <dt>Now</dt>
            <dd>{brief.currentStep}</dd>
          </>
        )}
      </dl>

      {card.remarks.map(remark => (
        <RemarkRow key={remark.id} remark={remark} closeRemark={closeRemark} />
      ))}

      {looseEnds.map(looseEnd => (
        <LooseEndRow key={looseEnd.id} looseEnd={looseEnd} answerLooseEnd={answerLooseEnd} dismissLooseEnd={dismissLooseEnd} />
      ))}

      {card.requests.length > 0 && <div className={styles.chatBriefRequests}>{card.requests.map(renderRequest)}</div>}

      {changes.length > 0 && (
        <details className={styles.chatBriefHistory}>
          <summary>What it changed · {changes.length}</summary>
          <ol>
            {changes.map(change => (
              <li key={change.id}>
                <span className={styles.chatBriefHistoryTime} title={change.createdAt}>{timeAgo(change.createdAt)}</span>{' '}
                {change.goalChanged && <strong>Goal changed to: {change.overallGoal}. </strong>}
                {change.turnChange}
              </li>
            ))}
          </ol>
        </details>
      )}
    </article>
  )
})

function LooseEndRow({
  looseEnd,
  answerLooseEnd,
  dismissLooseEnd,
}: {
  looseEnd: LooseEnd
  answerLooseEnd: (looseEnd: LooseEnd, text: string) => Promise<void>
  dismissLooseEnd: (looseEnd: LooseEnd) => Promise<void>
}) {
  const [answer, setAnswer] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = (work: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    work()
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setBusy(false))
  }
  return (
    <div className={styles.chatBriefLooseEnd} data-loose-end-id={looseEnd.id}>
      <div className={styles.chatBriefLooseEndHead}>
        <span className={styles.chatBriefLooseEndKind}>{looseEndKindLabel[looseEnd.kind]}</span>
        <span title={looseEnd.createdAt}>{timeAgo(looseEnd.createdAt)}</span>
      </div>
      <p className={styles.chatBriefLooseEndText}>{looseEnd.text}</p>
      {looseEnd.context !== '' && <p className={styles.chatBriefLooseEndContext}>{looseEnd.context}</p>}
      <div className={styles.chatBriefLooseEndActions}>
        <input
          type="text"
          className={styles.chatBriefLooseEndInput}
          placeholder="Answer — sent to the chat as a message"
          value={answer}
          disabled={busy}
          onChange={event => setAnswer(event.target.value)}
          onKeyDown={event => {
            if (event.key === 'Enter' && answer.trim() !== '') run(() => answerLooseEnd(looseEnd, answer.trim()))
          }}
        />
        <button
          type="button"
          className="bc-ctrl-btn"
          disabled={busy || answer.trim() === ''}
          onClick={() => run(() => answerLooseEnd(looseEnd, answer.trim()))}
        >
          Answer
        </button>
        <button type="button" className="bc-ctrl-btn" disabled={busy} onClick={() => run(() => dismissLooseEnd(looseEnd))}>
          Dismiss
        </button>
      </div>
      {error !== null && <p className={styles.inboxError}>{error}</p>}
    </div>
  )
}

function RemarkRow({ remark, closeRemark }: { remark: OrchestratorRemark; closeRemark: (remark: OrchestratorRemark) => Promise<void> }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  return (
    <div className={styles.chatBriefRemark} data-remark-id={remark.id}>
      <span className={styles.chatBriefLooseEndKind}>Orchestrator</span>
      <span className={styles.chatBriefRemarkText}>{remark.text}</span>
      <button
        type="button"
        className="bc-ctrl-btn"
        disabled={busy}
        onClick={() => {
          setBusy(true)
          setError(null)
          closeRemark(remark)
            .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
            .finally(() => setBusy(false))
        }}
      >
        Close
      </button>
      {error !== null && <p className={styles.inboxError}>{error}</p>}
    </div>
  )
}
