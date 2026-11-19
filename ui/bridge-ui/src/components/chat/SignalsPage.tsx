import { memo, useCallback, useMemo, useState, type ReactNode } from 'react'
import { openLooseEnds, useOpenSignalsByAttention, useSessionBriefs, useSessionNames, type Signal, type SignalRequest } from '@kayushkin/chat-core'
import { chatBriefSections } from '../../chatBriefSections'
import { ChatBriefCardView, useOrchestratorRemarks } from './ChatBriefCards'
import { SignalRequestCard } from './SignalCard'
import { RefChip } from './RefChip'
import { backgroundSessionTypeLabel, sessionTypeOfRequest, signalSection } from '../../signalAttentionSections'
import { timeAgo } from '../../utils'
import styles from './Chat.module.css'
import { PeerMessagesNeedingYou, useWaitingPeerMessages } from '../PeerMessageCards'
import { withoutPeerMessageSignals } from '../../peerMessages'
import { ConnectedServersPeerMessages, useConnectedServerWaitingCounts } from './ConnectedServersPeerMessages'

/** How many background cards the open background section draws at a time. There
 *  were 319 on 2026-10-20, nearly all notifications with a markdown body, and
 *  drawing them all at once froze the page for over a second. */
const BACKGROUND_PAGE_SIZE = 50

/**
 * Every open signal on a page of its own — the sidebar's "Needs you", opened out
 * so several can be read and answered in one sitting.
 *
 * The sidebar panel is capped at a third of the sidebar's height and draws compact
 * cards, which drop each signal's body and each option's description. That is right
 * for a glance and wrong for answering: the body is usually the part that says what
 * the question is about. Here every card is full size, and each carries what the
 * sidebar has no room for — when it was raised, by what, who it is for, the todo it
 * belongs to, and a reply triage drafted for the customer.
 *
 * Same read and same split as the sidebar (`useOpenSignalsByAttention()`), so the two
 * share one cache entry and a card answered here leaves the sidebar count too. "Needs
 * you" is the operator's (interactive and herald sessions); background agents'
 * signals follow in a collapsed section of their own, with their own count, whose
 * cards are drawn only once it is opened.
 */
export default function SignalsPage({
  onSelectSession,
  onSelectConnectedServerSession,
  backControl,
}: {
  /** Open the session a card belongs to. Must keep one identity across renders:
   *  every card is memoized on it. */
  onSelectSession: (sessionId: string) => void
  /** Open a session on a connected server: one a request waiting there started. */
  onSelectConnectedServerSession: (connectionId: string, sessionId: string) => void
  /** The header's way off the page, drawn by the host. */
  backControl: ReactNode
}) {
  const { operator, background, sessionTypeOf, available, loading, error, reload } = useOpenSignalsByAttention()
  // A message from a connected server is drawn by its own card, not as the
  // question the server raises for it.
  const operatorQuestions = useMemo(() => withoutPeerMessageSignals(operator), [operator])
  // Each chat's brief, the orchestrator's remarks on it and its open signals
  // are drawn as one card; the lists below hold only what no card drew.
  const sessionBriefs = useSessionBriefs()
  const orchestratorRemarks = useOrchestratorRemarks()
  const chats = useMemo(
    () => chatBriefSections(sessionBriefs.briefs, operatorQuestions.requests, orchestratorRemarks.remarks),
    [sessionBriefs.briefs, operatorQuestions.requests, orchestratorRemarks.remarks],
  )
  const [quietChatsOpen, setQuietChatsOpen] = useState(false)
  const needsYou = useMemo(() => {
    const outside = new Set(chats.requestsOutsideCards)
    const signalsOutside = new Set(chats.requestsOutsideCards.flatMap(request => request.signals))
    return signalSection({
      requests: operatorQuestions.requests.filter(request => outside.has(request)),
      signals: operatorQuestions.signals.filter(signal => signalsOutside.has(signal)),
    })
  }, [operatorQuestions, chats.requestsOutsideCards])
  const chatsWaitingCount = chats.waiting.reduce((sum, card) => sum + card.waitingCount, 0)
  const peerMessages = useWaitingPeerMessages()
  // What waits on each connected server this one administers, by connection id.
  const { total: connectedServerWaitingCount, report: reportConnectedServerWaiting } = useConnectedServerWaitingCounts()
  const backgroundSection = useMemo(() => signalSection(background), [background])
  const sessionIds = useMemo(
    () => [
      ...needsYou.requests.map(request => request.sessionId),
      ...backgroundSection.requests.map(request => request.sessionId),
      ...sessionBriefs.briefs.map(brief => brief.sessionId),
    ],
    [needsYou, backgroundSection, sessionBriefs.briefs],
  )
  const sessionName = useSessionNames(sessionIds)

  // A shut <details> still mounts its children, so the background cards are drawn
  // only once the section is opened, and then a page at a time.
  const [backgroundOpen, setBackgroundOpen] = useState(false)
  const [backgroundShown, setBackgroundShown] = useState(BACKGROUND_PAGE_SIZE)

  const renderRequest = (request: SignalRequest) => (
    <SignalsPageRequest
      // A derived group has no request id, so it is keyed by its one signal's id.
      key={request.requestId || request.signals[0]?.id}
      request={request}
      sessionName={sessionName(request.sessionId)}
      sessionType={sessionTypeOfRequest(request, sessionTypeOf)}
      onSelectSession={onSelectSession}
      onResolved={reload}
    />
  )

  const backgroundQuestions = backgroundSection.questionRequests.slice(0, backgroundShown)
  const backgroundNotifications = backgroundSection.notificationRequests.slice(
    0,
    Math.max(0, backgroundShown - backgroundSection.questionRequests.length),
  )
  const backgroundHidden = backgroundSection.requests.length - backgroundShown

  return (
    <div className={styles.signalsPage} role="region" aria-label="Signals needing you">
      <div className={styles.signalsPageHeader}>
        <h2 className={styles.signalsPageTitle}>Needs you</h2>
        <span className={styles.inboxCount}>{chatsWaitingCount + needsYou.signalCount + peerMessages.waiting.length + connectedServerWaitingCount}</span>
        <span className={styles.signalsPageSpacer} />
        <button
          type="button"
          className="bc-ctrl-btn"
          onClick={() => {
            reload()
            sessionBriefs.reload()
            orchestratorRemarks.reload()
          }}
          disabled={loading}
        >
          {loading ? 'Loading…' : 'Refresh'}
        </button>
        {backControl}
      </div>

      <div className={styles.signalsPageBody}>
        {error !== null && <p className={styles.inboxError}>Couldn’t load: {error}</p>}
        {!available && (
          <p className={styles.signalsPageEmpty}>This bridge server has no signals route.</p>
        )}
        {available && error === null && !loading && chats.waiting.length === 0 && needsYou.requests.length === 0 && peerMessages.waiting.length === 0 && connectedServerWaitingCount === 0 && (
          <p className={styles.signalsPageEmpty}>Nothing is waiting on you.</p>
        )}

        {(peerMessages.waiting.length > 0 || peerMessages.error !== null) && (
          <section className={styles.signalsPageSection}>
            <h3 className={styles.signalsPageSectionTitle}>From connected servers · {peerMessages.waiting.length}</h3>
            <PeerMessagesNeedingYou
              waiting={peerMessages.waiting}
              error={peerMessages.error}
              reload={peerMessages.reload}
              onSelectSession={onSelectSession}
            />
          </section>
        )}

        <ConnectedServersPeerMessages
          drawn
          countInHeading
          onWaitingCount={reportConnectedServerWaiting}
          onSelectSession={onSelectConnectedServerSession}
        />

        {sessionBriefs.error !== null && <p className={styles.inboxError}>Couldn’t load the chat briefs: {sessionBriefs.error}</p>}
        {orchestratorRemarks.error !== null && (
          <p className={styles.inboxError}>Couldn’t load the orchestrator’s remarks: {orchestratorRemarks.error}</p>
        )}
        {chats.waiting.length > 0 && (
          <section className={styles.signalsPageSection}>
            <h3 className={styles.signalsPageSectionTitle}>
              Chats · {chats.waiting.length}
              <span className={styles.signalsPageBackgroundNote}>
                {' '}— {chats.waiting.reduce((sum, card) => sum + openLooseEnds(card.brief).length, 0)} loose ends
              </span>
            </h3>
            {chats.waiting.map(card => (
              <ChatBriefCardView
                key={card.brief.sessionId}
                card={card}
                sessionName={sessionName(card.brief.sessionId)}
                onSelectSession={onSelectSession}
                renderRequest={renderRequest}
                answerLooseEnd={sessionBriefs.answerLooseEnd}
                dismissLooseEnd={sessionBriefs.dismissLooseEnd}
                closeRemark={orchestratorRemarks.close}
              />
            ))}
          </section>
        )}

        {needsYou.questionRequests.length > 0 && (
          <section className={styles.signalsPageSection}>
            <h3 className={styles.signalsPageSectionTitle}>Questions · {needsYou.questionRequests.length}</h3>
            {needsYou.questionRequests.map(renderRequest)}
          </section>
        )}
        {needsYou.notificationRequests.length > 0 && (
          <section className={styles.signalsPageSection}>
            <h3 className={styles.signalsPageSectionTitle}>
              Notifications · {needsYou.notificationRequests.length}
            </h3>
            {needsYou.notificationRequests.map(renderRequest)}
          </section>
        )}

        {chats.quiet.length > 0 && (
          <details
            className={styles.signalsPageBackground}
            open={quietChatsOpen}
            onToggle={event => setQuietChatsOpen(event.currentTarget.open)}
          >
            <summary className={styles.signalsPageBackgroundSummary}>
              Other chats · {chats.quiet.length}
              <span className={styles.signalsPageBackgroundNote}> — nothing waiting on you; their goals and what they changed</span>
            </summary>
            {quietChatsOpen && (
              <section className={styles.signalsPageSection}>
                {chats.quiet.map(card => (
                  <ChatBriefCardView
                    key={card.brief.sessionId}
                    card={card}
                    sessionName={sessionName(card.brief.sessionId)}
                    onSelectSession={onSelectSession}
                    renderRequest={renderRequest}
                    answerLooseEnd={sessionBriefs.answerLooseEnd}
                    dismissLooseEnd={sessionBriefs.dismissLooseEnd}
                    closeRemark={orchestratorRemarks.close}
                  />
                ))}
              </section>
            )}
          </details>
        )}

        {/* Background agents' signals: listed, never counted as needing you. A
            <details> that starts shut, so the page opens on the operator's own and the
            background pile is one line with its count until asked for. */}
        {backgroundSection.requests.length > 0 && (
          <details
            className={styles.signalsPageBackground}
            open={backgroundOpen}
            onToggle={event => setBackgroundOpen(event.currentTarget.open)}
          >
            <summary className={styles.signalsPageBackgroundSummary}>
              Background agents · {backgroundSection.signalCount}
              <span className={styles.signalsPageBackgroundNote}>
                {' '}— autoworkers, delegates, scheduled tasks and other background sessions; not counted as
                needing you
              </span>
            </summary>
            {backgroundOpen && backgroundQuestions.length > 0 && (
              <section className={styles.signalsPageSection}>
                <h3 className={styles.signalsPageSectionTitle}>
                  Questions · {backgroundSection.questionRequests.length}
                </h3>
                {backgroundQuestions.map(renderRequest)}
              </section>
            )}
            {backgroundOpen && backgroundNotifications.length > 0 && (
              <section className={styles.signalsPageSection}>
                <h3 className={styles.signalsPageSectionTitle}>
                  Notifications · {backgroundSection.notificationRequests.length}
                </h3>
                {backgroundNotifications.map(renderRequest)}
              </section>
            )}
            {backgroundOpen && backgroundHidden > 0 && (
              <button
                type="button"
                className="bc-ctrl-btn"
                onClick={() => setBackgroundShown(shown => shown + BACKGROUND_PAGE_SIZE)}
              >
                Show {Math.min(BACKGROUND_PAGE_SIZE, backgroundHidden)} more of {backgroundHidden}
              </button>
            )}
          </details>
        )}
      </div>
    </div>
  )
}

/**
 * One card and the line above it. Memoized, and given only values that keep their
 * identity while nothing about this card changes: the page re-renders on every
 * signals read and every resolved name, and re-drawing hundreds of markdown cards
 * each time is what made the page lag.
 */
const SignalsPageRequest = memo(function SignalsPageRequest({
  request,
  sessionName,
  sessionType,
  onSelectSession,
  onResolved,
}: {
  request: SignalRequest
  sessionName: string
  sessionType: string
  onSelectSession: (sessionId: string) => void
  onResolved: () => void
}) {
  const openSession = useCallback(() => onSelectSession(request.sessionId), [onSelectSession, request.sessionId])
  return (
    <SignalRequestCard
      request={request}
      onResolved={onResolved}
      // As in the sidebar inbox: the session that asked has usually stopped, and
      // without this the only way to clear the row is to answer a question that no
      // longer matters.
      allowDismissWithoutAnswer
      header={
        <SignalRequestDetails
          request={request}
          sessionName={sessionName}
          sessionType={sessionType}
          onOpenSession={openSession}
        />
      }
    />
  )
})

/**
 * The line above a card: which session, when, raised by what, for whom, and the todo
 * it belongs to — plus triage's drafted customer reply when there is one.
 *
 * Read off the request's first signal. The signals in one request come from one tool
 * call in one session, so they share all of these.
 */
function SignalRequestDetails({
  request,
  sessionName,
  sessionType,
  onOpenSession,
}: {
  request: SignalRequest
  sessionName: string
  /** The raising session's type as chat-core's split saw it, or '' when unknown. */
  sessionType: string
  onOpenSession: () => void
}) {
  const first: Signal | undefined = request.signals[0]
  if (!first) return null
  const draft = request.signals.find(signal => signal.customerReplyDraft !== null)?.customerReplyDraft

  return (
    <div className={styles.signalsPageDetails}>
      <div className={styles.signalsPageMeta}>
        <button
          type="button"
          className={styles.inboxSessionButton}
          title="Open this session"
          onClick={onOpenSession}
        >
          {sessionName}
        </button>
        <span title={first.createdAt}>{timeAgo(first.createdAt)}</span>
        {first.source !== '' && <span>from {first.source}</span>}
        {/* Always said, and an unknown type is said to be unknown: on this page the
            kind of session is what separates the operator's from a background agent's. */}
        <span>{backgroundSessionTypeLabel(sessionType)} session</span>
        {first.audience !== '' && <span>for the {first.audience}</span>}
        {first.linkedTodoId !== '' && <RefChip kind="todo" refId={first.linkedTodoId} />}
      </div>
      {draft && (
        <details className={styles.signalsPageDraft}>
          <summary>Drafted reply to the customer{draft.to !== '' ? ` (${draft.to})` : ''}</summary>
          <p className={styles.signalsPageDraftSubject}>{draft.subject}</p>
          <p className={styles.signalsPageDraftBody}>{draft.body}</p>
        </details>
      )}
    </div>
  )
}
