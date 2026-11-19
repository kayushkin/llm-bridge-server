import { useContext, useEffect, useLayoutEffect, useRef, useState, type JSX, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import type { RefKind } from '@kayushkin/chat-core';
import type { ManagedSessionDetail, SignalRequest, TurnModel } from '@kayushkin/chat-core';
import type { NoteboardItem } from '@kayushkin/chat-core';
import type { ResolvedRefMatch } from '@kayushkin/chat-core';
import type { Project } from '@kayushkin/project-store-types';
import type { Repo as RepoStoreRepo } from '@kayushkin/repo-store-types';
import type { LoggedMessage } from '@kayushkin/multichat-types';
import { messageChipLabel, messageConversationHref, messageOfAnswer } from '../../messageRefs';
import { GenericReferenceChip, RefusedReferenceChip } from './GenericReferenceChip';
import { BridgeContext, useBridgeConfig } from '../../context';
import { githubCommitUrl, shortSha, workGraphChatHref, type WorkGraphCommitAnswer } from '../../workGraph';
import {
  isSessionAwaitingOperator,
  loadSessionRefDetail,
  useNoteboardRefDetail,
  useResolvedRef,
  useSessionRefDetail,
  useSessionRefTranscript,
  REF_TRANSCRIPT_TURNS,
} from '@kayushkin/chat-core';
import { SignalRequestList } from './SessionSignals';
import { SessionChipOpenQuestions } from './SessionChipOpenQuestions';
import { useOpenSignalRequestsOfSession } from './sessionsWithOpenQuestion';
import { SessionActionInText } from './SessionActions';
import { FileRefChip } from './FileRefChip';
import { SessionFileChip, useSessionFileShown } from './SessionFileChip';
import {
  RefChipServerContext,
  refChipRoute,
  sessionLookupWasNotFound,
  useOpenConnectedServerSession,
  useRefChipServer,
} from './refChipServer';
import { OnConnectedServer, useOptionalConnectedServers } from './ConnectedServers';
import { connectionOfServerId } from '../../serverListings';

/** Props for the {@link RefChip} renderer. When used as a ReactMarkdown custom
 *  component the mdast hProperties (`kind` / `refId`) arrive as props; rehype may
 *  lowercase the attribute, so `refid` is accepted too. Extra props are ignored. */
export interface RefChipProps {
  kind?: RefKind | string;
  refId?: string;
  /** Lowercased alias react-markdown/rehype may pass instead of `refId`. */
  refid?: string;
  className?: string;
  children?: ReactNode;
  /**
   * Navigate to the referenced thing — for a session, open its chat.
   *
   * The chip BODY never calls this: for every kind it opens the detail panel,
   * which is the common "what is this?" glance. Navigation is the deliberate
   * act, so it lives on the ↗ button beside a session chip and on the panel's
   * "Open session" button — both rendered only when this handler exists, so an
   * affordance never points nowhere. Noteboard kinds never invoke it at all
   * (nothing on this fleet deep-links to one item); the previous arrangement
   * called it for every kind and dash answered only `session`, which left todo
   * chips announcing themselves as buttons and doing nothing.
   */
  onActivate?: (kind: string, refId: string) => void;
  /** The `server_…` id of the server the record is on, when the text wrote
   *  `<id>@<server_id>`; rehype may lowercase it to `serverid`. */
  serverId?: string;
  serverid?: string;
  [key: string]: unknown;
}

/**
 * A reference chip: a bare session id or a cue-prefixed noteboard uuid, found in
 * message text by `remarkRefChips` and rendered here as a labeled chip with a
 * detail panel.
 *
 * Wire it into ReactMarkdown as the `ref-chip` component alongside
 * `remarkRefChips`:
 *
 *   <ReactMarkdown
 *     remarkPlugins={[remarkRefChips]}
 *     components={{ 'ref-chip': RefChip }}
 *   >{text}</ReactMarkdown>
 *
 * ⚠️ This component reads from `ChatProvider`'s context (it resolves the id
 * against llm-bridge or noteboard), so it must be mounted inside one. It used to
 * be a pure `<span>` that could stand alone; it is not anymore.
 *
 * Theming still stays at the edge. Every element carries a stable, unhashed
 * class name (`ref-chip-*`) and `data-*` attributes, and this file ships no CSS
 * — the host styles it, as dash does in `Chat.module.css` via `:global()`.
 */
export function RefChip(props: RefChipProps): JSX.Element {
  const kind = String(props.kind ?? 'session');
  const refId = String(props.refId ?? props.refid ?? '');
  const server = useRefChipServer();

  // A malformed node with no id resolves to nothing; render the raw text rather
  // than an empty chip so the message loses no content.
  if (!refId) return <>{String(props.children ?? '')}</>;

  // `<id>@<server_id>`: drawn against the server that holds it.
  const serverId = String(props.serverId ?? props.serverid ?? '');
  if (serverId) {
    return <ServerQualifiedRefChip refId={refId} kind={kind} serverId={serverId} className={props.className} onActivate={props.onActivate} />;
  }

  // Inside a connected server's thread the id is that server's: never look it
  // up here (refChipServer.ts says why).
  const route = refChipRoute(kind, server);
  if (server && route === 'remote-lookup') {
    return <RemoteLookupRefChip refId={refId} kind={kind} serverName={server.serverName} className={props.className} />;
  }
  if (server && route === 'remote-neutral') {
    return <RemoteRefChip refId={refId} kind={kind} serverName={server.serverName} />;
  }
  if (server && route === 'remote-session') {
    const openSession = server.openSession;
    return (
      <SessionRefChip
        refId={refId}
        className={props.className}
        serverName={server.serverName}
        onActivate={openSession ? (_kind, sessionId) => openSession(sessionId) : props.onActivate}
      />
    );
  }

  if (kind === 'session') {
    return (
      <SessionRefChip refId={refId} className={props.className} onActivate={props.onActivate} />
    );
  }
  if (kind === 'file') {
    // A code span naming a file; plain code unless the session's tools named it.
    return <FileRefChip mentionText={refId} className={props.className} />;
  }
  if (kind === 'action') {
    // A button this session's agent offered and placed here by writing its id.
    return <SessionActionInText actionId={refId} />;
  }
  if (kind === 'registered') {
    // The session's own shared-file ids are registered patterns too (TurnList adds
    // them); one of those draws as the file's chip, anything else goes to the resolver.
    return <RegisteredRefChip refId={refId} className={props.className} onActivate={props.onActivate} />;
  }
  if (kind === 'uuid') {
    // A bare uuid, or an id of a type the resolver registered (`project_000001`):
    // either way the resolver says what it names.
    return (
      <UnclassifiedRefChip refId={refId} className={props.className} onActivate={props.onActivate} />
    );
  }
  return <NoteboardRefChip refId={refId} kind={kind} className={props.className} />;
}

function RegisteredRefChip({
  refId,
  className,
  onActivate,
}: {
  refId: string;
  className?: string;
  onActivate?: (kind: string, refId: string) => void;
}): JSX.Element {
  // A file shared into the session on screen: its own chip, with the file's name
  // beside the id that was written.
  const shown = useSessionFileShown(refId);
  if (shown) {
    return <SessionFileChip file={shown.file} personName={shown.personName} label={shown.file.filename} className={className} />;
  }
  return <UnclassifiedRefChip refId={refId} className={className} onActivate={onActivate} />;
}

/** Gap in px between the chip and its panel, and the margin the panel keeps
 *  from the viewport edge when it has to be nudged back inside. */
const PANEL_GAP = 4;
const VIEWPORT_MARGIN = 8;

/**
 * Open/close plus placement for the detail panel.
 *
 * ⚠️ The panel is rendered through a PORTAL onto document.body, and that is
 * load-bearing rather than stylistic. dash renders turns through `virtua`,
 * whose list-item wrapper carries `contain: layout style` — which creates a
 * stacking context. A panel positioned inside that item therefore has its
 * `z-index` scoped to the item, and the very next turn's prose paints straight
 * over it: measured, the element at the panel's own center point was a later
 * turn's `<p>`, so the buttons could be seen and not clicked. No z-index on the
 * panel can fix that from inside the containing block; escaping it can. The
 * portal also escapes the scroll container's `overflow`, which would otherwise
 * clip a panel opened on the last visible turn.
 *
 * The cost of the portal is that placement becomes manual: the panel is
 * `position: fixed` and anchored to the chip's measured rect, re-measured on
 * scroll and resize. `capture: true` on the scroll listener is required — the
 * turns list is an inner scroller and scroll events do not bubble to window.
 */
export function useAnchoredPanel(): {
  open: boolean;
  toggle: () => void;
  wrapRef: React.RefObject<HTMLSpanElement | null>;
  panelRef: React.RefObject<HTMLDivElement | null>;
  panelStyle: { top: number; left: number } | null;
} {
  const [open, setOpen] = useState(false);
  const [panelStyle, setPanelStyle] = useState<{ top: number; left: number } | null>(null);
  const wrapRef = useRef<HTMLSpanElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);

  useLayoutEffect(() => {
    if (!open) {
      setPanelStyle(null);
      return;
    }
    const place = (): void => {
      const anchor = wrapRef.current;
      if (!anchor) return;
      const rect = anchor.getBoundingClientRect();
      // Measured when it exists; on the first pass it does not, and the fallback
      // only has to be close enough to avoid a visible jump.
      const panelHeight = panelRef.current?.offsetHeight ?? 260;
      const panelWidth = panelRef.current?.offsetWidth ?? 280;

      // Below the chip, unless that would run off the bottom and there is more
      // room above — the usual dropdown flip.
      const roomBelow = window.innerHeight - rect.bottom;
      const roomAbove = rect.top;
      const placeAbove = roomBelow < panelHeight + VIEWPORT_MARGIN && roomAbove > roomBelow;
      const rawTop = placeAbove ? rect.top - panelHeight - PANEL_GAP : rect.bottom + PANEL_GAP;

      const maxTop = window.innerHeight - panelHeight - VIEWPORT_MARGIN;
      const maxLeft = window.innerWidth - panelWidth - VIEWPORT_MARGIN;
      setPanelStyle({
        top: Math.max(VIEWPORT_MARGIN, Math.min(rawTop, Math.max(VIEWPORT_MARGIN, maxTop))),
        left: Math.max(VIEWPORT_MARGIN, Math.min(rect.left, Math.max(VIEWPORT_MARGIN, maxLeft))),
      });
    };

    place();
    // capture:true — the turns list scrolls internally and its scroll events
    // never reach window on the bubble phase.
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => {
      window.removeEventListener('scroll', place, true);
      window.removeEventListener('resize', place);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDocPointerDown = (e: MouseEvent): void => {
      const target = e.target as Node;
      // The panel is no longer a descendant of the wrapper, so "outside" has to
      // clear BOTH or every click inside the panel would close it.
      const insideChip = wrapRef.current?.contains(target) ?? false;
      // A chip inside this panel opens its own panel in another portal, which
      // the DOM does not place inside this one; React's events still pass
      // through this panel on the way up, and AnchoredPanel marks them.
      const insidePanel =
        (panelRef.current?.contains(target) ?? false) ||
        (panelRef.current !== null && (panelsAMouseDownPassedThrough.get(e)?.has(panelRef.current) ?? false));
      if (!insideChip && !insidePanel) setOpen(false);
    };
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onDocPointerDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDocPointerDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  return { open, toggle: () => setOpen((o) => !o), wrapRef, panelRef, panelStyle };
}

/** The panels a mousedown passed through in React's tree, which reaches a
 *  panel's own handler before the document's listener sees the event. A chip
 *  in a panel portals its own panel elsewhere in the DOM, so this is how a
 *  click in that inner panel keeps the outer one open. */
const panelsAMouseDownPassedThrough = new WeakMap<Event, Set<HTMLDivElement>>();

/** The portaled panel shell. Kept in one place so both chips share the
 *  placement, the ref wiring and the dialog semantics. */
export function AnchoredPanel({
  panelRef,
  panelStyle,
  label,
  refId,
  refKind,
  children,
}: {
  panelRef: React.RefObject<HTMLDivElement | null>;
  panelStyle: { top: number; left: number } | null;
  label: string;
  refId: string;
  refKind: string;
  children: ReactNode;
}): JSX.Element {
  return createPortal(
    <div
      ref={panelRef}
      className="ref-chip-panel"
      role="dialog"
      aria-label={label}
      // The portal puts this at the end of <body>, far from the chip it belongs
      // to, so it carries the same identifying attributes as the chip wrapper.
      // Without them the DOM cannot say which reference an open panel describes.
      data-ref-id={refId}
      data-ref-kind={refKind}
      onMouseDown={(event) => {
        const panel = panelRef.current;
        if (!panel) return;
        const passed = panelsAMouseDownPassedThrough.get(event.nativeEvent) ?? new Set<HTMLDivElement>();
        passed.add(panel);
        panelsAMouseDownPassedThrough.set(event.nativeEvent, passed);
      }}
      // Placement is inline because it is measured, not themeable. Everything
      // else about the panel's appearance stays in the host's stylesheet.
      // `visibility` hides the first, unmeasured paint rather than letting it
      // flash at the wrong place.
      style={{
        top: panelStyle?.top ?? 0,
        left: panelStyle?.left ?? 0,
        visibility: panelStyle ? 'visible' : 'hidden',
      }}
    >
      {children}
    </div>,
    document.body,
  );
}

export function truncate(s: string, n = 28): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

/** Last `n` characters of an id, for a chip whose target has no name yet. */
export function idTail(id: string, n = 12): string {
  return id.length > n ? `…${id.slice(-n)}` : id;
}

/** Relative time, for rows where "how long ago" is the question. Falls back to
 *  the raw string when the timestamp does not parse — never to "now", which
 *  would be a fabricated answer. */
export function timeAgo(iso: string): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return iso;
  const seconds = Math.round((Date.now() - then) / 1000);
  const future = seconds < 0;
  const abs = Math.abs(seconds);
  const [value, unit] =
    abs < 60
      ? [abs, 'second']
      : abs < 3600
        ? [Math.round(abs / 60), 'minute']
        : abs < 86400
          ? [Math.round(abs / 3600), 'hour']
          : [Math.round(abs / 86400), 'day'];
  const plural = value === 1 ? '' : 's';
  return future ? `in ${value} ${unit}${plural}` : `${value} ${unit}${plural} ago`;
}

function formatUsd(usd: number): string {
  return usd < 0.01 && usd > 0 ? `<$0.01` : `$${usd.toFixed(2)}`;
}

/**
 * The chip's emoji, categorizing a session the way the filter bar does: by
 * stored `type`, refined with `purpose` and the id prefix — herald and
 * autoworker are purposes of an autonomous session, not distinct types.
 * `external` means the session ran outside the bridge and was imported by
 * scanning harness history, so nobody declared a purpose and it must not read
 * as a human chat.
 */
function sessionEmoji(type: string, purpose: string, sessionId: string): string {
  if (type === 'herald' || purpose === 'herald' || sessionId.startsWith('herald-')) return '📣';
  if (purpose === 'autoworker' || sessionId.startsWith('autoworker-')) return '🤖';
  if (type === 'interactive') return '💬';
  if (type === 'system') return '⚙️';
  if (type === 'autonomous') return '🛠️';
  if (type === 'external') return '📥';
  return '🔗';
}

/** The "needs a human" states, badged so a waiting question stands out — only on a
 *  session the operator is to answer (`isSessionAwaitingOperator`). A background
 *  session in the same state shows the state with no badge. */
const STATE_BADGES: ReadonlyMap<string, string> = new Map([
  ['awaiting_user', 'question'],
  ['awaiting_permission', 'approval'],
]);

// ---------------------------------------------------------------------------
// Session chip
// ---------------------------------------------------------------------------

function SessionRefChip({
  refId,
  className,
  onActivate,
  serverName,
}: {
  refId: string;
  className?: string;
  onActivate?: (kind: string, refId: string) => void;
  /** Set when the session lives on a connected server: the chip says so. */
  serverName?: string;
}): JSX.Element {
  const { data, error, loading } = useSessionRefDetail(refId);
  // A session id this server does not have may be one of a connected server's,
  // written into this server's thread (an agent here reporting on the laptop,
  // say). Session ids are minted from the clock, not counted, so looking the
  // same id up on the connected servers cannot land on a different session.
  if (!serverName && sessionLookupWasNotFound(error)) {
    return <SessionOnConnectedServer refId={refId} className={className} />;
  }
  return (
    <SessionRefChipBody
      refId={refId}
      className={className}
      onActivate={onActivate}
      serverName={serverName}
      detail={{ data, error, loading }}
    />
  );
}

/** A session id this server answered 404 for: ask each connected server that
 *  can be read now, in the order the sidebar lists them, and draw the chip
 *  against the first that has it. With none, the chip stays as this server
 *  drew it — a session it could not find. */
function SessionOnConnectedServer({ refId, className }: { refId: string; className?: string }): JSX.Element {
  const servers = useOptionalConnectedServers();
  const openConnectedServerSession = useOpenConnectedServerSession();
  const [found, setFound] = useState<{ connectionId: string; serverName: string } | null>(null);
  const [searched, setSearched] = useState(false);
  const candidates = servers
    ? servers.servers.filter((folder) => folder.blockedReason === null && servers.storeOf(folder.connectionId))
    : [];
  const candidateKey = candidates.map((folder) => folder.connectionId).join(' ');

  useEffect(() => {
    if (!servers) { setSearched(true); return; }
    let live = true;
    setFound(null);
    setSearched(false);
    void (async () => {
      for (const folder of candidates) {
        const store = servers.storeOf(folder.connectionId);
        if (!store) continue;
        try {
          await loadSessionRefDetail(store.api, refId);
          if (live) setFound({ connectionId: folder.connectionId, serverName: folder.label });
          return;
        } catch {
          // Not on that server, or it could not answer: try the next one.
        }
      }
      if (live) setSearched(true);
    })();
    return () => { live = false; };
    // The candidates are named by `candidateKey`; their store objects are read
    // through `servers` when the search runs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refId, candidateKey]);

  if (found) {
    const openSession = openConnectedServerSession
      ? (sessionId: string) => openConnectedServerSession({ connectionId: found.connectionId, sessionId })
      : undefined;
    return (
      <OnConnectedServer connectionId={found.connectionId}>
        <RefChipServerContext.Provider value={{ connectionId: found.connectionId, serverName: found.serverName, openSession }}>
          <SessionRefChip
            refId={refId}
            className={className}
            serverName={found.serverName}
            onActivate={openSession ? (_kind, sessionId) => openSession(sessionId) : undefined}
          />
        </RefChipServerContext.Provider>
      </OnConnectedServer>
    );
  }
  return (
    <SessionRefChipBody
      refId={refId}
      className={className}
      detail={{
        data: null,
        error: searched ? 'this server has no such session, and no connected server has it either' : null,
        loading: !searched,
      }}
    />
  );
}

/** A record id in a connected server's transcript, looked up by that
 *  server's reference-store: this chip is mounted under that server's store,
 *  so the lookup goes through its gateway. A server that cannot answer — one
 *  with no reference-store yet, or a person it does not let read it — leaves
 *  the neutral chip, which says where the id is from. */
function RemoteLookupRefChip({ refId, kind, serverName, className }: { refId: string; kind: string; serverName: string; className?: string }): JSX.Element {
  const { data: matches, error, loading } = useResolvedRef(refId);
  if (loading || error || !matches || matches.length === 0) {
    return <RemoteRefChip refId={refId} kind={kind} serverName={serverName} />;
  }
  return <UnclassifiedRefChip refId={refId} className={className} />;
}

/** `<id>@<server_id>`: a record on the server whose own id the text names.
 *  This server's own id draws the plain chip; a connected server's draws under
 *  that server's store, as in its own thread; a server this one is not
 *  connected to, or cannot read now, draws the neutral chip with its id. */
function ServerQualifiedRefChip({
  refId,
  kind,
  serverId,
  className,
  onActivate,
}: {
  refId: string;
  kind: string;
  serverId: string;
  className?: string;
  onActivate?: (kind: string, refId: string) => void;
}): JSX.Element {
  const servers = useOptionalConnectedServers();
  const openConnectedServerSession = useOpenConnectedServerSession();
  if (servers?.thisServerId === serverId) {
    return <RefChip kind={kind} refId={refId} className={className} onActivate={onActivate} />;
  }
  const connection = servers ? connectionOfServerId(servers.servers, serverId) : null;
  if (!servers || !connection || connection.blockedReason !== null || !servers.storeOf(connection.connectionId)) {
    const why = connection?.blockedReason ?? `this server is not connected to ${serverId}`;
    return (
      <span className="ref-chip ref-chip-remote" data-ref-kind={kind} data-ref-id={refId} data-ref-server={serverId} title={`${refId} — on ${connection?.label ?? serverId}: ${why}`}>
        <span className="ref-chip-glyph" aria-hidden>🖥</span>
        <span className="ref-chip-label">{refId}</span>
        <span className="ref-chip-server">{connection?.label ?? serverId}</span>
      </span>
    );
  }
  const openSession = openConnectedServerSession
    ? (sessionId: string) => openConnectedServerSession({ connectionId: connection.connectionId, sessionId })
    : undefined;
  return (
    <OnConnectedServer connectionId={connection.connectionId}>
      <RefChipServerContext.Provider value={{ connectionId: connection.connectionId, serverName: connection.label, openSession }}>
        <RefChip kind={kind} refId={refId} className={className} />
      </RefChipServerContext.Provider>
    </OnConnectedServer>
  );
}

/** An id from a connected server's transcript that this server has no way to
 *  look up: it says what it is and where it is from, and opens nothing. Drawn
 *  muted and dashed so it never passes for one of this server's records. */
function RemoteRefChip({ refId, kind, serverName }: { refId: string; kind: string; serverName: string }): JSX.Element {
  return (
    <span
      className="ref-chip ref-chip-remote"
      data-ref-kind={kind}
      data-ref-id={refId}
      data-ref-server={serverName}
      title={`${refId} — from ${serverName}; this server cannot look it up`}
    >
      <span className="ref-chip-glyph" aria-hidden>
        🖥
      </span>
      <span className="ref-chip-label">{refId}</span>
      <span className="ref-chip-server">{serverName}</span>
    </span>
  );
}

function SessionRefChipBody({
  refId,
  className,
  onActivate,
  serverName,
  detail,
}: {
  refId: string;
  className?: string;
  onActivate?: (kind: string, refId: string) => void;
  serverName?: string;
  detail: { data: ManagedSessionDetail | null; error: string | null; loading: boolean };
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const { data, error, loading } = detail;
  // The one cross-session /signals read every chip and the sidebar share, not a
  // read per chip: see `useOpenSignalRequestsOfSession`.
  const openSignals = useOpenSignalRequestsOfSession(refId);
  const [answerFormOpen, setAnswerFormOpen] = useState(false);
  const hasOpenQuestion = openSignals.questionRequests.length > 0;

  const summary = data?.summary;
  const emoji = summary ? sessionEmoji(summary.type, summary.purpose, refId) : '💬';
  const label = summary?.displayName ? truncate(summary.displayName) : idTail(refId);
  const name = serverName ? `${summary?.displayName || refId} — on ${serverName}` : summary?.displayName || refId;

  // The chip BODY opens the detail panel; navigation lives on the ↗ button beside
  // it. It used to be the other way around — body navigated, a separate ▾ caret
  // opened the panel — and the user asked for the inversion: a glance at what a
  // reference names is the common case, leaving the thread is the deliberate one.
  // The body therefore matches the noteboard chip's shape (one button, inline
  // caret), and the ↗ renders only when a navigation handler exists — same rule
  // as before, an affordance never points nowhere.
  //
  // A session with an open question carries a `?` on the chip and its question in
  // the text right after it (`SessionChipOpenQuestions`), answerable there. The
  // chip and that text are siblings, not one element, so the panel still anchors
  // to the chip alone.
  return (
    <>
    <span
      className={`ref-chip-wrap${hasOpenQuestion ? ' ref-chip-wrap-has-question' : ''}`}
      ref={wrapRef}
      data-ref-kind="session"
      data-ref-id={refId}
      data-ref-server={serverName}
    >
      <button
        type="button"
        className={`${className ?? 'ref-chip ref-chip-session'} ref-chip-item${serverName ? ' ref-chip-on-server' : ''}${open ? ' ref-chip-open' : ''}${hasOpenQuestion ? ' ref-chip-has-question' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={hasOpenQuestion ? `${name} — waiting on your answer` : name}
      >
        <span className="ref-chip-glyph" aria-hidden>
          {emoji}
        </span>
        <span className="ref-chip-label">{label}</span>
        {serverName && <span className="ref-chip-server">{serverName}</span>}
        {hasOpenQuestion && (
          <span className="ref-chip-question-marker" aria-label="has an open question">
            ?
          </span>
        )}
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {onActivate && (
        <button
          type="button"
          className="ref-chip-open-session"
          onClick={() => onActivate('session', refId)}
          aria-label={`Open chat — ${summary?.displayName || refId}`}
          title={`Open chat — ${summary?.displayName || refId}`}
        >
          ↗
        </button>
      )}
      {open && (
        <AnchoredPanel
          panelRef={panelRef}
          panelStyle={panelStyle}
          label="Session details"
          refId={refId}
          refKind="session"
        >
          {loading && <div className="ref-chip-panel-loading">Loading session…</div>}
          {error && <div className="ref-chip-panel-error">Couldn’t load session: {error}</div>}
          {data && (
            <SessionRefPanel
              detail={data}
              refId={refId}
              onActivate={onActivate}
              hasOpenQuestion={hasOpenQuestion}
              panelRequests={openSignals.panelRequests}
              onSignalsResolved={openSignals.reload}
            />
          )}
        </AnchoredPanel>
      )}
    </span>
    {/* Said, not swallowed: without the read nothing here can say whether the
        session is waiting on you, and drawing a plain chip would say it is not. */}
    {openSignals.error !== null && (
      <span
        className="ref-chip-open-questions-error"
        title={`Couldn’t check this session for open questions: ${openSignals.error}`}
      >
        (couldn’t check for questions)
      </span>
    )}
    <SessionChipOpenQuestions
      sessionId={refId}
      questionRequests={openSignals.questionRequests}
      answerFormOpen={answerFormOpen}
      onToggleAnswerForm={() => setAnswerFormOpen((wasOpen) => !wasOpen)}
      onResolved={openSignals.reload}
    />
    </>
  );
}

function SessionRefPanel({
  detail,
  refId,
  onActivate,
  hasOpenQuestion,
  panelRequests,
  onSignalsResolved,
}: {
  detail: ManagedSessionDetail;
  refId: string;
  onActivate?: (kind: string, refId: string) => void;
  hasOpenQuestion: boolean;
  /** The session's open notifications, and every open group of a background
   *  session. The operator's questions are drawn beside the chip, not here — see
   *  `SessionChipOpenQuestions` and `sessionChipSignalShares`. */
  panelRequests: readonly SignalRequest[];
  onSignalsResolved: () => void;
}): JSX.Element {
  const [showHistory, setShowHistory] = useState(false);
  const { summary, info } = detail;

  return (
    <>
      <div className="ref-chip-panel-title">{summary.displayName || '(untitled session)'}</div>

      <RefRow
        label="State"
        value={summary.state}
        badge={isSessionAwaitingOperator(summary) ? STATE_BADGES.get(summary.state) : undefined}
      />
      <RefRow
        label="Type"
        value={summary.purpose ? `${summary.type} · ${summary.purpose}` : summary.type}
      />
      {summary.harness !== '' && <RefRow label="Harness" value={summary.harness} />}
      {info?.model && <RefRow label="Model" value={info.model} />}
      {info?.permissionMode && <RefRow label="Permissions" value={info.permissionMode} />}
      {summary.mode !== '' && <RefRow label="Mode" value={summary.mode} />}
      {summary.folderName !== '' && <RefRow label="Folder" value={summary.folderName} />}
      {summary.instanceId !== '' && <RefRow label="Instance" value={summary.instanceId} />}
      {/* Working dir is reported in two places and they can disagree: the
          harness's own view (`info.workingDir`) and the managed session's
          (`detail.workingDir`). Prefer the harness's, which is where the agent
          is actually running. */}
      {(info?.workingDir ?? detail.workingDir) && (
        <RefRow label="Working dir" value={(info?.workingDir ?? detail.workingDir) as string} />
      )}
      {detail.origin && <RefRow label="Origin" value={detail.origin} />}
      {detail.pid !== undefined && <RefRow label="PID" value={String(detail.pid)} />}
      {/* Spend is reported, never derived. An absent figure means the server
          sent none, which is not the same as zero, so no row is drawn. */}
      {detail.spendUsd !== undefined && (
        <RefRow
          label="Spend"
          value={
            detail.maxBudgetUsd !== undefined
              ? `${formatUsd(detail.spendUsd)} of ${formatUsd(detail.maxBudgetUsd)}`
              : formatUsd(detail.spendUsd)
          }
        />
      )}
      {detail.forkedFromSessionId && (
        <RefRow label="Forked from" value={idTail(detail.forkedFromSessionId, 14)} />
      )}
      {detail.managerSessionId && (
        <RefRow label="Manager" value={idTail(detail.managerSessionId, 14)} />
      )}
      {info?.tools && info.tools.length > 0 && (
        <RefRow label="Tools" value={String(info.tools.length)} />
      )}
      {info?.mcpServers && info.mcpServers.length > 0 && (
        <RefRow label="MCP" value={info.mcpServers.map((s) => s.name).join(', ')} />
      )}
      {info?.skills && info.skills.length > 0 && (
        <RefRow label="Skills" value={info.skills.join(', ')} />
      )}
      <RefRow label="Created" value={timeAgo(summary.createdAt)} />
      <RefRow label="Updated" value={timeAgo(summary.updatedAt)} />

      <div className="ref-chip-panel-actions">
        <button
          type="button"
          className="ref-chip-panel-btn"
          onClick={() => setShowHistory((v) => !v)}
          aria-expanded={showHistory}
        >
          {showHistory ? 'Hide history' : `Load last ${REF_TRANSCRIPT_TURNS} turns`}
        </button>
        {onActivate && (
          <button
            type="button"
            className="ref-chip-panel-btn"
            onClick={() => onActivate('session', refId)}
          >
            Open session
          </button>
        )}
      </div>

      {/* A question is answered beside the chip, where it shows without a click;
          drawing its form here too would put two live forms for one question on
          the page. The panel keeps the session's notifications, and a background
          session's questions, which nothing beside the chip draws. Both come from the shared cross-session read, so
          opening a panel puts no request of its own on the wire. */}
      {hasOpenQuestion && (
        <div className="ref-chip-panel-note">
          This session is waiting on your answer — it is shown beside the chip.
        </div>
      )}
      <SignalRequestList requests={panelRequests} compact onResolved={onSignalsResolved} />

      {/* Mounted only while expanded, which is also what gates the fetch. */}
      {showHistory && <SessionRefTranscript refId={refId} />}
    </>
  );
}

function SessionRefTranscript({ refId }: { refId: string }): JSX.Element {
  const { data, error, loading } = useSessionRefTranscript(refId, true);

  if (loading) return <div className="ref-chip-panel-loading">Loading history…</div>;
  if (error) return <div className="ref-chip-panel-error">Couldn’t load history: {error}</div>;
  if (!data) return <div className="ref-chip-panel-loading">No history.</div>;
  return <TranscriptBody model={data} />;
}

/**
 * The referenced session's recent turns, as plain text.
 *
 * Deliberately NOT markdown: chat-core carries no markdown renderer (dash owns
 * that pipeline, with its own plugins), and reaching for one here would both add
 * a dependency this package refuses and risk recursion — a transcript rendered
 * through `remarkRefChips` would linkify the ids inside it into more chips, each
 * able to load another transcript.
 */
function TranscriptBody({ model }: { model: TurnModel }): JSX.Element {
  const rows = model.turns.map((turn) => {
    // The collapsed view's rule: primary, non-duplicate entries only, so an
    // OTel copy of a message does not print twice.
    const text = turn.entryIds
      .map((id) => model.entries[id])
      .filter((e): e is NonNullable<typeof e> => e !== undefined)
      .filter((e) => !e.duplicate && e.primary && e.kind === 'text' && e.text)
      .map((e) => e.text as string)
      .join('\n');
    return { id: turn.id, role: turn.role, ts: turn.ts, text };
  });
  const withText = rows.filter((r) => r.text !== '');

  return (
    <div className="ref-chip-transcript">
      {/* `more` is the server's own word for "older turns exist beyond this
          page". Said plainly, so a reader never mistakes this window for the
          whole session. */}
      {model.more && (
        <div className="ref-chip-transcript-note">
          Showing the last {REF_TRANSCRIPT_TURNS} turns — older ones exist. Open the session for
          the rest.
        </div>
      )}
      {withText.length === 0 && (
        <div className="ref-chip-panel-loading">No prose turns in this window.</div>
      )}
      {withText.map((r) => (
        <div key={r.id} className="ref-chip-transcript-turn" data-role={r.role}>
          <div className="ref-chip-transcript-role">
            {r.role} · {timeAgo(r.ts)}
          </div>
          <div className="ref-chip-transcript-text">{r.text}</div>
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Noteboard chip (note / todo / rank / workspace)
// ---------------------------------------------------------------------------

/** Emoji for a noteboard item. Held and deleted outrank the item's type,
 *  because they are what a reader most needs to know about a quoted id: a
 *  parked or deleted item is not work anyone should pick up. */
function itemEmoji(item: NoteboardItem): string {
  if (item.deleted_at) return '🗑';
  if (item.held_at) return '⏸';
  if (item.status === 'done') return '✅';
  if (item.type === 'note') return '📝';
  if (item.type === 'workspace') return '🧠';
  if (item.type === 'rank') return '🔢';
  return '☑';
}

function NoteboardRefChip({
  refId,
  kind,
  className,
}: {
  refId: string;
  kind: string;
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const { data: item, error, loading } = useNoteboardRefDetail(refId);

  // The cue word only decided which regex matched; the loaded item's own `type`
  // is the authority, so the rendered kind switches to it as soon as it lands.
  const resolvedKind = item?.type ?? kind;
  const label = item?.title ? truncate(item.title) : idTail(refId);
  const emoji = item ? itemEmoji(item) : '☑';

  return (
    <span
      className="ref-chip-wrap"
      ref={wrapRef}
      data-ref-kind={resolvedKind}
      data-ref-id={refId}
    >
      {/* Nothing on this fleet deep-links to a single noteboard item, so the
          whole chip opens the panel rather than pretending to navigate. */}
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={`${resolvedKind} — ${item?.title || refId}`}
      >
        <span className="ref-chip-glyph" aria-hidden>
          {emoji}
        </span>
        <span className="ref-chip-label">{label}</span>
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel
          panelRef={panelRef}
          panelStyle={panelStyle}
          label={`${resolvedKind} details`}
          refId={refId}
          refKind={resolvedKind}
        >
          {loading && <div className="ref-chip-panel-loading">Loading {kind}…</div>}
          {error && <div className="ref-chip-panel-error">Couldn’t load {kind}: {error}</div>}
          {item && <NoteboardRefPanel item={item} />}
        </AnchoredPanel>
      )}
    </span>
  );
}

function NoteboardRefPanel({ item }: { item: NoteboardItem }): JSX.Element {
  const [showBody, setShowBody] = useState(false);

  // Deleted and held are not statuses (noteboard keeps them as separate stamps
  // precisely so a restore can tell them apart), so they are badged onto the
  // status row rather than replacing its value.
  const statusBadge = item.deleted_at ? 'deleted' : item.held_at ? 'held' : undefined;

  return (
    <>
      <div className="ref-chip-panel-title">{item.title || '(untitled)'}</div>

      <RefRow label="Type" value={item.type} />
      <RefRow label="Status" value={item.status} badge={statusBadge} />
      {item.priority !== 0 && <RefRow label="Priority" value={String(item.priority)} />}
      {item.tags.length > 0 && <RefRow label="Tags" value={item.tags.join(', ')} />}
      {item.list_id !== '' && <RefRow label="List" value={item.list_id} />}
      {item.parent_id && <RefRow label="Parent" value={idTail(item.parent_id, 12)} />}
      {item.due_at && <RefRow label="Due" value={timeAgo(item.due_at)} />}
      {/* The rule, not a second due date — `due_at` above stays the answer to
          "when next". Shown because a recurring item behaves quite differently
          from a one-off and nothing else on the chip would say so. */}
      {item.schedule?.rrule && (
        <RefRow
          label="Repeats"
          value={
            item.schedule.tzid
              ? `${item.schedule.rrule} (${item.schedule.tzid})`
              : item.schedule.rrule
          }
        />
      )}
      {item.schedule?.remind?.nag && <RefRow label="Nags" value={item.schedule.remind.nag} />}
      {item.hold_reason && <RefRow label="Held because" value={item.hold_reason} />}
      {item.auto_hold_at_usd !== undefined && (
        <RefRow label="Spend ceiling" value={formatUsd(item.auto_hold_at_usd)} />
      )}
      {item.links.length > 0 && <RefRow label="Links" value={String(item.links.length)} />}
      {item.created_by !== '' && <RefRow label="Created by" value={item.created_by} />}
      <RefRow label="Created" value={timeAgo(item.created_at)} />
      <RefRow label="Updated" value={timeAgo(item.updated_at)} />

      {/* The body arrived with the item — one GET returns the whole row — so
          this expands what is already in hand and costs no request. */}
      {item.body !== '' && (
        <>
          <div className="ref-chip-panel-actions">
            <button
              type="button"
              className="ref-chip-panel-btn"
              onClick={() => setShowBody((v) => !v)}
              aria-expanded={showBody}
            >
              {showBody ? 'Hide body' : 'Show full body'}
            </button>
          </div>
          {showBody && <div className="ref-chip-item-body">{item.body}</div>}
        </>
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// Unclassified chip (a bare uuid, classified through the host's resolver)
// ---------------------------------------------------------------------------

/**
 * A bare uuid detected with no cue word. The surrounding text says nothing
 * about what it names, so the host's reference resolver is asked, and the chip
 * re-renders as whichever kind the id turns out to be:
 *
 *  - one `session` match → the session chip;
 *  - one `note` match (noteboard's one id space — note, todo, rank,
 *    workspace) → the noteboard chip, which labels itself from the item;
 *  - several matches, or a type this renderer has no chip for → a generic chip
 *    whose panel lists every match, because picking one silently would be a
 *    guess presented as a fact;
 *  - no match, no resolver, or a resolver error → the id as plain text, which
 *    is exactly what the message showed before detection existed. An error
 *    carries a tooltip so the failure is discoverable without being noisy.
 */
function UnclassifiedRefChip({
  refId,
  className,
  onActivate,
}: {
  refId: string;
  className?: string;
  onActivate?: (kind: string, refId: string) => void;
}): JSX.Element {
  const { data: matches, error, loading } = useResolvedRef(refId);
  // Optional: a chip drawn with no BridgeProvider above it has no host chips.
  const renderReferenceChip = useContext(BridgeContext)?.renderReferenceChip ?? null;

  if (loading || matches === null || matches.length === 0) {
    return (
      <span data-ref-kind="uuid" data-ref-id={refId} title={error ?? undefined}>
        {refId}
      </span>
    );
  }
  if (matches.length === 1) {
    const match = matches[0] as ResolvedRefMatch;
    if (match.refused) {
      return <RefusedReferenceChip refId={refId} match={match} />;
    }
    // The host's own chip first, for a record of one of its stores.
    const hostChip = renderReferenceChip?.({ refId, match, className });
    if (hostChip) return <>{hostChip}</>;
    if (match.type === 'session') {
      return <SessionRefChip refId={refId} className={className} onActivate={onActivate} />;
    }
    if (match.type === 'note') {
      // The fetched item's own `type` labels the chip; the match's registry
      // type only says which store answered.
      const itemType = (match.data as { type?: string } | null)?.type ?? 'note';
      return <NoteboardRefChip refId={refId} kind={itemType} className={className} />;
    }
    if (match.type === 'project') {
      return <ProjectRefChip refId={refId} project={match.data as Project} className={className} />;
    }
    if (match.type === 'message') {
      const message = messageOfAnswer(match.data);
      if (message) return <MessageRefChip refId={refId} message={message} className={className} />;
    }
    if (match.type === 'repo') {
      return <RepoRefChip refId={refId} repo={match.data as RepoStoreRepo} className={className} />;
    }
    if (match.type === 'commit') {
      return (
        <CommitRefChip
          refId={refId}
          answer={match.data as WorkGraphCommitAnswer}
          className={className}
          onActivate={onActivate}
        />
      );
    }
    // Any other type, or one whose chip did not recognize the answer: drawn
    // from its row in reference-store's list.
    return <GenericReferenceChip refId={refId} match={match} className={className} />;
  }
  return <MultiMatchRefChip refId={refId} matches={matches} className={className} />;
}

/** The honest rendering for an id that resolved ambiguously (several stores
 *  recognize it) or to a type this renderer has no dedicated chip for: a chip
 *  whose panel lists every match, so the reader does the picking. */
export function MultiMatchRefChip({
  refId,
  matches,
  className,
}: {
  refId: string;
  matches: ResolvedRefMatch[];
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  // A uuid is long and says nothing, so its tail is enough to tell two apart; a
  // registered id (`prediction_000267`) is short and names its own type, so it
  // is shown whole.
  const shownId = UUID_SHAPE.test(refId) ? idTail(refId) : refId;
  const label =
    matches.length === 1 && UUID_SHAPE.test(refId) ? `${matches[0]?.type} ${shownId}` : shownId;

  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind="resolved" data-ref-id={refId}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={refId}
      >
        <span className="ref-chip-glyph" aria-hidden>
          🔗
        </span>
        <span className="ref-chip-label">{label}</span>
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel
          panelRef={panelRef}
          panelStyle={panelStyle}
          label="Reference details"
          refId={refId}
          refKind="resolved"
        >
          <div className="ref-chip-panel-title">{refId}</div>
          {matches.length > 1 && (
            <div className="ref-chip-panel-loading">
              This id resolves in {matches.length} stores:
            </div>
          )}
          {matches.map((m) => (
            <RefRow key={`${m.service}/${m.type}`} label={m.type} value={m.service} />
          ))}
        </AnchoredPanel>
      )}
    </span>
  );
}

const UUID_SHAPE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** A project id, resolved by project-store: the project's name on the chip, and
 *  its stage, goal and id in the panel, with a link that opens the project's
 *  page in the chat's projects view. The link is a plain one: the view reads
 *  `?view=projects&project=` when the chat loads. */
function ProjectRefChip({
  refId,
  project,
  className,
}: {
  refId: string;
  project: Project;
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const { routes } = useBridgeConfig();
  const href = routes.chat
    ? `${routes.chat}?view=projects&project=${encodeURIComponent(refId)}`
    : '';
  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind="project" data-ref-id={refId}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={`${project.name} (${refId})`}
      >
        <span className="ref-chip-glyph" aria-hidden>
          ▦
        </span>
        <span className="ref-chip-label">{truncate(project.name || refId)}</span>
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label="Project details" refId={refId} refKind="project">
          <div className="ref-chip-panel-title">{project.name}</div>
          <RefRow label="Id" value={refId} />
          <RefRow label="Kind" value={project.kind} />
          <RefRow label="Stage" value={project.stage.replace(/_/g, ' ')} />
          {project.goal && <RefRow label="Goal" value={project.goal} />}
          {href && (
            <div className="ref-chip-panel-actions">
              <a className="ref-chip-panel-btn" href={href}>
                Open project
              </a>
            </div>
          )}
        </AnchoredPanel>
      )}
    </span>
  );
}

/** A message id (`message_000123`), resolved by multichat's message log: who
 *  sent it and the day on the chip, and in the panel the conversation, the
 *  time and the text, with a link to the conversation on the Messages page. */
function MessageRefChip({
  refId,
  message,
  className,
}: {
  refId: string;
  message: LoggedMessage;
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const { routes, multichatBasePath } = useBridgeConfig();
  const href = multichatBasePath ? messageConversationHref(routes.messageConversations, message.room_id) : '';
  const label = messageChipLabel(message);
  const deleted = message.redacted_at !== null;
  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind="message" data-ref-id={refId}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={`${label} (${refId})`}
      >
        <span className="ref-chip-glyph" aria-hidden>
          💬
        </span>
        <span className="ref-chip-label">{truncate(label)}</span>
        {deleted && <span className="ref-chip-badge ref-chip-badge-deleted">deleted</span>}
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label="Message details" refId={refId} refKind="message">
          <div className="ref-chip-panel-title">{label}</div>
          <RefRow label="Id" value={refId} badge={deleted ? 'deleted' : undefined} />
          {message.room_name && <RefRow label="Chat" value={message.room_name} />}
          <RefRow label="App" value={message.platform} />
          <RefRow label="Sent" value={new Date(message.sent_at).toLocaleString()} />
          {message.body && <div className="ref-chip-panel-body">{message.body}</div>}
          {href && (
            <div className="ref-chip-panel-actions">
              <a className="ref-chip-panel-btn" href={href}>
                Open conversation
              </a>
            </div>
          )}
        </AnchoredPanel>
      )}
    </span>
  );
}

/** A `repo:<name>` mention, resolved by repo-store: the repo's name on the
 *  chip, and in the panel its path and remote, with links to its GitHub page
 *  and to the chat's work graph for it. */
function RepoRefChip({
  refId,
  repo,
  className,
}: {
  refId: string;
  repo: RepoStoreRepo;
  className?: string;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const { routes } = useBridgeConfig();
  const workGraphHref = routes.chat ? workGraphChatHref(routes.chat, repo.id) : '';
  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind="repo" data-ref-id={refId}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={repo.path ? `${repo.name} (${repo.path})` : repo.name}
      >
        <span className="ref-chip-glyph" aria-hidden>
          ⎇
        </span>
        <span className="ref-chip-label">{truncate(repo.name)}</span>
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label="Repository details" refId={refId} refKind="repo">
          <div className="ref-chip-panel-title">{repo.name}</div>
          {repo.description && <RefRow label="About" value={repo.description} />}
          {repo.path && <RefRow label="Path" value={repo.path} />}
          {repo.git_remote && <RefRow label="Remote" value={repo.git_remote} />}
          {(repo.github_url || workGraphHref) && (
            <div className="ref-chip-panel-actions">
              {repo.github_url && (
                <a className="ref-chip-panel-btn" href={repo.github_url} target="_blank" rel="noreferrer">
                  GitHub ↗
                </a>
              )}
              {workGraphHref && (
                <a className="ref-chip-panel-btn" href={workGraphHref}>
                  Work graph
                </a>
              )}
            </div>
          )}
        </AnchoredPanel>
      )}
    </span>
  );
}

/** A `<repo>@<sha>` mention, resolved by work-graph-store: `repo@sha` on the
 *  chip, and in the panel the commit's subject, author and maker, with links to
 *  the commit on GitHub (only when origin has it, since GitHub would 404
 *  otherwise) and in the chat's work graph. */
function CommitRefChip({
  refId,
  answer,
  className,
  onActivate,
}: {
  refId: string;
  answer: WorkGraphCommitAnswer;
  className?: string;
  onActivate?: (kind: string, refId: string) => void;
}): JSX.Element {
  const { open, toggle, wrapRef, panelRef, panelStyle } = useAnchoredPanel();
  const { routes } = useBridgeConfig();
  const { repo, commit } = answer;
  const githubHref = githubCommitUrl(repo, commit);
  const workGraphHref = routes.chat ? workGraphChatHref(routes.chat, repo.id, commit.sha) : '';
  const madeBy = commit.made_by;
  return (
    <span className="ref-chip-wrap" ref={wrapRef} data-ref-kind="commit" data-ref-id={refId}>
      <button
        type="button"
        className={`${className ?? 'ref-chip'} ref-chip-item${open ? ' ref-chip-open' : ''}`}
        onClick={toggle}
        aria-expanded={open}
        title={`${commit.subject}\n${repo.name} ${commit.sha}`}
      >
        <span className="ref-chip-glyph" aria-hidden>
          ⊙
        </span>
        <span className="ref-chip-label">
          {repo.name}@{shortSha(commit.sha)}
        </span>
        <span className="ref-chip-caret-inline" aria-hidden>
          ▾
        </span>
      </button>
      {open && (
        <AnchoredPanel panelRef={panelRef} panelStyle={panelStyle} label="Commit details" refId={refId} refKind="commit">
          <div className="ref-chip-panel-title">{commit.subject}</div>
          <RefRow label="Repo" value={repo.name} />
          <RefRow label="Sha" value={commit.sha} badge={commit.on_origin_branch ? undefined : 'unpushed'} />
          <RefRow label="Author" value={commit.author_name} />
          <RefRow label="Committed" value={timeAgo(new Date(commit.committed_at * 1000).toISOString())} />
          {madeBy && <RefRow label="Made by" value={`${madeBy.session_id} (git ${madeBy.git_subcommand})`} />}
          {(githubHref || workGraphHref || (madeBy && onActivate)) && (
            <div className="ref-chip-panel-actions">
              {githubHref && (
                <a className="ref-chip-panel-btn" href={githubHref} target="_blank" rel="noreferrer">
                  GitHub ↗
                </a>
              )}
              {workGraphHref && (
                <a className="ref-chip-panel-btn" href={workGraphHref}>
                  Work graph
                </a>
              )}
              {madeBy && onActivate && (
                <button type="button" className="ref-chip-panel-btn" onClick={() => onActivate('session', madeBy.session_id)}>
                  Open maker's session
                </button>
              )}
            </div>
          )}
        </AnchoredPanel>
      )}
    </span>
  );
}

// ---------------------------------------------------------------------------

export function RefRow({
  label,
  value,
  badge,
}: {
  label: string;
  value: string;
  badge?: string;
}): JSX.Element {
  return (
    <div className="ref-chip-panel-row">
      <span className="ref-chip-panel-label">{label}</span>
      <span className="ref-chip-panel-value">
        {value}
        {badge && <span className={`ref-chip-badge ref-chip-badge-${badge}`}>{badge}</span>}
      </span>
    </div>
  );
}
