import type { Entry, EntryKind, Role, ToolResultImage, Turn, TurnModel, Validator } from '../net/types.js';
import type { WireEvent, WireEventData } from '../net/wireEvents.js';
import { annotateOTelDuplicates } from './otelDedup.js';
// The ONE answer to "what tool id does this entry carry", shared with the pairing
// helpers rather than re-derived here — two readers of the same fact that disagree
// is how a call and its result stop finding each other.
import { toolIdOf } from '../store/toolPairing.js';

// LIVE-TAIL reducer. Settled history is materialized once, server-side; this
// only folds the small stream of live SSE events onto an already-finished
// TurnModel. Every operation is O(1)-indexed by key (Map lookups, never
// findIndex), and every event is deduped by its log-store `event_id` so a
// Last-Event-ID replay is idempotent. It KEEPS every copy (both sources) and
// re-runs `annotateOTelDuplicates` so the dedup annotations stay correct as the
// late OTel copy arrives.

/** Reducer working state for one session's live tail. The wire `TurnModel` is
 *  kept in sync on `model`; `turnIndex` and `seenEventIds` are the O(1) indexes
 *  that keep application cheap and idempotent. */
export interface TailState {
  sessionId: string;
  model: TurnModel;
  /** turnId → index into `model.turns`, so turn lookup is O(1). */
  turnIndex: Map<string, number>;
  /** entry id → the STREAM frame ids folded in (llm-bridge-server's own row ids
   *  off the SSE `id:` line). ⚠️ This is NOT the id space the materialized page
   *  uses: /messages carries LOG-STORE row ids, and the two number the same
   *  events differently (discovered 2026-08-25 — session br_1787615605129568013
   *  streamed ids ~1.77M while its page carried ~2.09M). Never compare a stream
   *  id with a page eventId. A non-empty set is also the honest marker that an
   *  entry was live-folded rather than materialized. */
  entryEventIds: Map<string, Set<number>>;
  /** every STREAM frame id applied, so a replayed frame is a no-op. Same space
   *  warning as above: page eventIds must never be added here — a live frame
   *  whose stream id equals a loaded page id would be silently swallowed. */
  seenEventIds: Set<number>;
}

const EMPTY_VALIDATOR: Validator = { maxEventId: 0, eventCount: 0, updatedAt: '' };

/** Keep a known cost/context roll-up alive across a page that arrived without one.
 *
 *  `TurnModel.aggregates` is PER-PAGE on the wire: log-store computes it last-value-wins
 *  over the events it happened to return, and omits the block entirely when that page
 *  held no spend or usage event. Its own comment spells out whose job the gap is:
 *  "the client falls back to its live-tail values". Nobody was doing that, so the header
 *  read $0.00 / no context bar the moment a page came back without spend on it — not
 *  because the session had spent nothing, but because this page had not seen the event
 *  that said so.
 *
 *  So the client treats the roll-up as SESSION state under the same last-value-wins rule
 *  the server uses within a page: the newest figure that actually arrived wins, and a
 *  page carrying no figure changes nothing. That never fabricates — every number on
 *  screen was genuinely reported for this session — and it never freezes either: a
 *  compaction that shrinks the window emits a fresh usage event, and the fresh (lower)
 *  value simply wins.
 *
 *  Direction is the whole reason both models are named rather than merged positionally:
 *  `fresher` wins whenever it carries a block at all, and `staler` only ever fills a
 *  gap. Get that backwards in `prependOlder` and paging backwards through a transcript
 *  walks the cost chip back in time. */
export function carryForwardAggregates(
  staler: TurnModel | undefined,
  fresher: TurnModel,
): TurnModel {
  if (fresher.aggregates || !staler?.aggregates) return fresher;
  return { ...fresher, aggregates: staler.aggregates };
}

/**
 * Keep the reasoning the live stream produced across a page that carries none.
 *
 * Same shape and same justification as `carryForwardAggregates` above: a materialized
 * page must not erase what it is structurally unable to report.
 *
 * ⚠️ Reasoning TEXT does not survive into storage, and the loss is upstream of this
 * whole stack. Measured on this host: Claude Code's own transcript records 607 thinking
 * blocks for one session with `thinking: ""` and a 472–724 byte signature — it records
 * that reasoning happened and signs it, never what it said. log-store therefore holds
 * 98,959 thinking blocks of which ZERO carry text, and reasoning text last appeared in
 * quantity in 2026-04 (70 events), then 3 in May and none since.
 *
 * So the live stream is the ONLY place a session's reasoning ever exists. Replacing the
 * live model with a materialized page — which `setTurns` does on every validator repair
 * and every session open — deleted it. That is the reported bug in all three of its
 * shapes: the aside vanishing when the final text lands, an open aside collapsing when
 * more text streams in (the entry is gone, so the element is unmounted), and reasoning
 * appearing to stop at the latest block.
 *
 * The rule is deliberately narrow. An entry is carried forward only when it is
 * `thinking`, carries text, and the fresher page has no entry under that id. Nothing
 * else is preserved: for every other kind the server IS authoritative, and a page that
 * drops a text entry is reporting a real edit — a compaction, a redaction — that the
 * client must honor. Carried entries keep their place in their turn, and a turn the
 * fresher page does not contain is not resurrected: it is off the loaded window and
 * renders nothing either way.
 *
 * This does not fabricate. Every preserved character was streamed to this client by the
 * server for this session. It is the client declining to throw away the only copy.
 */
export function carryForwardReasoning(
  staler: TurnModel | undefined,
  fresher: TurnModel,
): TurnModel {
  if (!staler) return fresher;

  const fresherTurnIds = new Set(fresher.turns.map((t) => t.id));
  const preserved: Record<string, Entry> = {};
  for (const id of Object.keys(staler.entries)) {
    const entry = staler.entries[id];
    if (!entry || entry.kind !== 'thinking' || !entry.text) continue;
    if (fresher.entries[id]) continue;
    if (!entry.turnId || !fresherTurnIds.has(entry.turnId)) continue;
    preserved[id] = entry;
  }
  const preservedIds = Object.keys(preserved);
  if (preservedIds.length === 0) return fresher;

  // Re-seated by eventId, not appended. The Turns view joins a turn's reasoning in
  // `entryIds` order, so appending would put earlier reasoning after later reasoning
  // in the same aside — right text, wrong order, and nothing on screen to say so.
  const byTurn = new Map<string, string[]>();
  for (const id of preservedIds) {
    const turnId = preserved[id]!.turnId!;
    const list = byTurn.get(turnId);
    if (list) list.push(id);
    else byTurn.set(turnId, [id]);
  }
  const eventIdOfEntry = (id: string): number =>
    fresher.entries[id]?.eventId ?? preserved[id]?.eventId ?? 0;

  return {
    ...fresher,
    entries: { ...fresher.entries, ...preserved },
    turns: fresher.turns.map((turn) => {
      const extra = byTurn.get(turn.id);
      if (!extra) return turn;
      const merged = [...turn.entryIds, ...extra].sort(
        (a, b) => eventIdOfEntry(a) - eventIdOfEntry(b),
      );
      return { ...turn, entryIds: merged };
    }),
  };
}

/** Build a fresh, empty tail state, or seed it from a server-materialized
 *  TurnModel (the warm/cold-switch path: reduce only the tail on top of this). */
export function initTailState(sessionId: string, model?: TurnModel): TailState {
  const base: TurnModel = model ?? {
    sessionId,
    turns: [],
    entries: {},
    validator: { ...EMPTY_VALIDATOR },
    more: false,
  };
  const turnIndex = new Map<string, number>();
  base.turns.forEach((t, i) => turnIndex.set(t.id, i));
  // The model is a server page, or a cached model: pages plus live rows that kept
  // their own stamp. Whatever carries none came off a page.
  const stamped: Record<string, Entry> = {};
  for (const [id, entry] of Object.entries(base.entries)) stamped[id] = stampedAsPage(entry);
  const stampedBase: TurnModel = { ...base, entries: stamped };
  // ⚠️ Deliberately EMPTY. These maps hold STREAM frame ids; the model's
  // eventIds are log-store ids — a different numbering of the same events.
  // Seeding seenEventIds from the page used to swallow any live frame whose
  // stream id numerically equalled a loaded page id, which is one of the three
  // faces of the two-id-space bug.
  return {
    sessionId,
    model: stampedBase,
    turnIndex,
    entryEventIds: new Map<string, Set<number>>(),
    seenEventIds: new Set<number>(),
  };
}

/** Was this entry folded from the live stream (or born in the client)? Reads the
 *  stamp the fold writes — see `Entry.origin` for why it is not the frame-id map. */
export function isLiveEntry(entry: Entry): boolean {
  return entry.origin === 'live';
}

/** The `before` cursor for the next older page: the smallest log-store eventId among
 *  entries a page delivered, or null when no page entry is held. Live entries carry
 *  llm-bridge-server's own row ids, a different and smaller numbering, so counting
 *  them asked for "older than" an id below the whole session: the server answered
 *  no turns and `more: false`, and the chat stopped offering older history
 *  (br_1791424129618218898, 2026-10-08). */
export function olderPageCursor(model: TurnModel): number | null {
  let oldest = Number.POSITIVE_INFINITY;
  for (const entry of Object.values(model.entries)) {
    if (!isLiveEntry(entry) && entry.eventId < oldest) oldest = entry.eventId;
  }
  return Number.isFinite(oldest) ? Math.floor(oldest) : null;
}

/** Stamp an entry a server page delivered. An entry that already says where it came
 *  from keeps its object, so a cached live row stays live and row memos keep hitting. */
function stampedAsPage(entry: Entry): Entry {
  if (entry.origin) return entry;
  // A page lists a tool result's images without their bytes; stamp each with the
  // event it came from while this entry still IS that event. See ToolResultImage.
  const toolResultImages = entry.toolResultImages?.map((image) =>
    image.eventId !== undefined || image.base64Data !== undefined ? image : { ...image, eventId: entry.eventId },
  );
  return { ...entry, origin: 'page', ...(toolResultImages ? { toolResultImages } : {}) };
}

/** A live tool result's images, with their bytes. A block held by URL rather than
 *  base64 is not drawn: the browser would fetch a URL a tool chose, from the
 *  dashboard's origin. */
function liveToolResultImages(
  content: NonNullable<WireEvent['data']['tool_result']>['content'],
): ToolResultImage[] {
  const images: ToolResultImage[] = [];
  (content ?? []).forEach((block, index) => {
    const source = block.image_block?.source;
    if (block.type !== 'image' || source?.kind !== 'base64' || !source.data) return;
    images.push({ index, mediaType: source.media_type ?? '', base64Data: source.data });
  });
  return images;
}

/** Trim + collapse whitespace, the correlation form for optimistic-user matching.
 *  One definition, used by both the SSE-time strip (ChatStore) and the merge-time
 *  supersession below, so the two paths cannot drift. */
export function normalizeText(s: string): string {
  return s.replace(/\s+/g, ' ').trim();
}

/** Whether an incoming materialized entry is the SAME content as one already held, so
 *  the held object can be reused and row memos keep hitting. The compared scalars are
 *  sufficient: a materialized entry is a pure function of its stored event rows (same
 *  id + same eventId ⇒ same payload) plus the annotation passes, whose outputs are
 *  exactly the annotation fields compared here. Object-typed fields (raw, toolInput,
 *  toolResult, usage) derive from the same stored event and need no deep compare. */
function sameMaterializedEntry(held: Entry, incoming: Entry): boolean {
  return (
    held.eventId === incoming.eventId &&
    held.kind === incoming.kind &&
    held.role === incoming.role &&
    held.source === incoming.source &&
    held.text === incoming.text &&
    held.ts === incoming.ts &&
    held.turnId === incoming.turnId &&
    held.duplicate === incoming.duplicate &&
    held.primary === incoming.primary &&
    held.groupId === incoming.groupId &&
    held.messageId === incoming.messageId &&
    held.toolName === incoming.toolName &&
    held.subtype === incoming.subtype
  );
}

/**
 * Fold a freshly materialized page into the session's tail state — MERGE, never
 * replace (dash docs/chat-turns-per-message.md §6). `setTurns` used to swap the
 * whole model and rebuild the tail from the incoming page alone, which is the
 * "everything resets" bug.
 *
 * ⚠️ THE JOIN IS SHARED IDENTITY, NEVER NUMBERS. The stream and the page number
 * the same events in two different id spaces (SSE `id:` = llm-bridge-server's own
 * rows; page eventId = log-store rows), so any numeric comparison across the
 * boundary is meaningless — an event-id join was tried first and kept every live
 * entry beside its page copy, doubling the transcript. What the two paths truly
 * share is the canonical identifiers ON the events: `turnId`, `messageId`,
 * `tool_id`, `taskId`, the timestamp. Those are the join:
 *
 *  - a live entry (`origin: 'live'` — the stamp, never the stream-id set, which a
 *    cache round trip empties) is superseded when the page
 *    REPORTS its content unit: same messageId+role+kind for message content;
 *    same tool_id (call, and result when the live entry holds one) for tools;
 *    same subtype+correlator for task system events; same kind+subtype+second
 *    for id-less bookkeeping. Unreported live entries are kept — dropping them
 *    is the "everything resets" bug;
 *  - carve-out: a live `thinking` entry WITH TEXT is kept even when reported —
 *    the materialized copy is structurally unable to carry the text;
 *  - an OPTIMISTIC user row is superseded when the page reports the real prompt
 *    (client request id, else normalized text);
 *  - a page entry (`origin: 'page'`) not in this page is
 *    off-window history and stays; one in the page takes the page's version,
 *    reusing the held object when the content is unchanged so memos keep hitting.
 *
 * ORDER never crosses the id spaces either: prior materialized-only turns, and
 * live turns that began before the page's oldest turn, keep their place before the
 * page's turns, the page's turns keep the page's order, other live-only turns come
 * last; within a shared turn the page's entries come first
 * and kept live entries after, each side in its own order. Sorting the mix by
 * eventId put newer live entries (small stream-flavoured ids) ABOVE older
 * materialized ones — the original "narration arrives out of order" report.
 *
 * The tail keeps its stream-space bookkeeping: surviving live entries keep their
 * folded frame-id sets, `seenEventIds` carries over UNCHANGED (page ids are the
 * wrong space and must never enter it).
 */
export function mergeMaterializedPage(
  prior: TailState | undefined,
  incoming: TurnModel,
): TailState {
  if (
    !prior ||
    (prior.model.turns.length === 0 && Object.keys(prior.model.entries).length === 0)
  ) {
    return initTailState(incoming.sessionId, incoming);
  }

  // --- Page indexes over the SHARED identifiers. ---
  const pageMessageUnits = new Set<string>(); // `${messageId}|${role}|${kind}`
  const pageToolCalls = new Set<string>();
  const pageToolResults = new Set<string>();
  const pageSystemUnits = new Set<string>(); // `${subtype}|${taskId or toolUseId}`
  const pageBookkeeping = new Set<string>(); // `${kind}|${subtype}|${epoch-second}`
  // `${turnId}|${normalizedText}` for user prompts. A prompt is on the wire twice
  // (bridge copy + OTel echo, different messageIds) and the page may carry EITHER
  // as its primary copy — a window floor that landed on the echo leaves the bridge's
  // copy outside the page. Joining user text on messageId alone then keeps the live
  // bridge copy as "unreported" and orders it after the page's entries, i.e. after
  // the answer (observed live 2026-09-02, br_1788370653337509270). The prompt is one
  // content unit however many ids it travels under, so it joins on text, per turn.
  const pageUserPrompts = new Set<string>();
  const epochSecondOf = (ts: string | undefined): number | null => {
    if (!ts) return null;
    const ms = Date.parse(ts);
    return Number.isNaN(ms) ? null : Math.floor(ms / 1000);
  };
  for (const inc of Object.values(incoming.entries)) {
    if (inc.messageId) pageMessageUnits.add(`${inc.messageId}|${inc.role}|${inc.kind}`);
    if (inc.role === 'user' && inc.kind === 'text' && inc.text) {
      pageUserPrompts.add(`${inc.turnId ?? ''}|${normalizeText(inc.text)}`);
    }
    const incToolId = toolIdOf(inc);
    if (inc.kind === 'tool_call' && incToolId) pageToolCalls.add(incToolId);
    if (inc.kind === 'tool_result' && incToolId) pageToolResults.add(incToolId);
    if (inc.kind === 'system' && inc.subtype && (inc.taskId || inc.toolUseId)) {
      pageSystemUnits.add(`${inc.subtype}|${inc.taskId || inc.toolUseId}`);
    }
    const sec = epochSecondOf(inc.ts);
    if (sec !== null) pageBookkeeping.add(`${inc.kind}|${inc.subtype ?? ''}|${sec}`);
  }

  /** Does the page report this live entry's content unit? */
  const pageReports = (held: Entry): boolean => {
    if (held.role === 'user' && held.kind === 'text' && held.text) {
      if (pageUserPrompts.has(`${held.turnId ?? ''}|${normalizeText(held.text)}`)) return true;
    }
    if (held.kind === 'tool_call' || held.kind === 'tool_result') {
      const toolId = toolIdOf(held);
      if (toolId) {
        const callReported = pageToolCalls.has(toolId);
        const resultReported = held.toolResult === undefined || pageToolResults.has(toolId);
        return callReported && resultReported;
      }
      // An unpairable tool entry: fall through to messageId, then keep.
    }
    if (held.messageId) {
      return pageMessageUnits.has(`${held.messageId}|${held.role}|${held.kind}`);
    }
    if (held.kind === 'system' && held.subtype && (held.taskId || held.toolUseId)) {
      return pageSystemUnits.has(`${held.subtype}|${held.taskId || held.toolUseId}`);
    }
    // Id-less bookkeeping (session_state, api_call, meta): kind+subtype+second.
    // The page truncates ts to seconds (RFC3339) while the live copy keeps
    // sub-second precision, so equality is on the epoch second. Over-dropping a
    // same-second twin costs a Raw-view row until reload; under-dropping doubles
    // it forever — the cheaper error is chosen.
    const sec = epochSecondOf(held.ts);
    if (sec !== null) return pageBookkeeping.has(`${held.kind}|${held.subtype ?? ''}|${sec}`);
    return false;
  };

  // --- Partition the held entries. ---
  const keptHeldIds: string[] = [];
  const liveHeldIds = new Set<string>(); // survivors that were live-folded (or optimistic)
  for (const id of Object.keys(prior.model.entries)) {
    const held = prior.model.entries[id];
    if (!held) continue;
    if (incoming.entries[id]) continue; // same id: the page's version wins below
    const heldRaw = held.raw as { optimistic?: boolean; clientId?: string } | undefined;
    if (heldRaw?.optimistic) {
      const normHeld = normalizeText(held.text ?? '');
      const reported = Object.values(incoming.entries).some((inc) => {
        if (inc.role !== 'user' || !inc.text) return false;
        // `clientRequestId` is the promoted field; the raw branch covers a log-store
        // that predates it. The chat sends the id it drew the optimistic row under
        // (`ApiClient.send`), and the bridge keeps it on the `user_message` it
        // records, so this id match is what pairs them. The normalized-text match
        // below is left for a message sent by a client that sends no id.
        const incClientRequestId =
          inc.clientRequestId ?? (inc.raw as { client_request_id?: string } | undefined)?.client_request_id;
        if (heldRaw.clientId && incClientRequestId === heldRaw.clientId) return true;
        return !!normHeld && normalizeText(inc.text) === normHeld;
      });
      if (reported) continue;
      keptHeldIds.push(id);
      liveHeldIds.add(id); // client-born: orders with the live tail, never as history
      continue;
    }
    if (!isLiveEntry(held)) {
      // Previously-materialized, off this page's window: history, kept as-is.
      keptHeldIds.push(id);
      continue;
    }
    const reasoningCarveOut = held.kind === 'thinking' && !!held.text;
    if (reasoningCarveOut || !pageReports(held)) {
      keptHeldIds.push(id);
      liveHeldIds.add(id);
    }
  }

  // --- Entries: the page's (identity-reusing), plus the kept. ---
  const entries: Record<string, Entry> = {};
  for (const id of Object.keys(incoming.entries)) {
    const inc = incoming.entries[id]!;
    const held = prior.model.entries[id];
    entries[id] = held && sameMaterializedEntry(held, inc) ? held : stampedAsPage(inc);
  }
  for (const id of keptHeldIds) entries[id] = prior.model.entries[id]!;

  // --- Turns, ordered without ever comparing across id spaces. ---
  const keptByTurn = new Map<string, string[]>();
  for (const id of keptHeldIds) {
    const turnId = entries[id]!.turnId;
    const list = keptByTurn.get(turnId);
    if (list) list.push(id);
    else keptByTurn.set(turnId, [id]);
  }
  const incomingTurnIds = new Set(incoming.turns.map((t) => t.id));
  // The page's oldest turn start, in epoch ms. A turn with live entries that began
  // before it fell off the page's window rather than arrived after it, so it is
  // history. Its live entries are often id-less ticks no page ever reports, which
  // kept it "live" for good and sank it below the page once the session outgrew one
  // page (br_1791424129618218898, 2026-10-08). Timestamps are shared by both paths.
  const pageStartMs = Math.min(
    ...incoming.turns.map((t) => Date.parse(t.ts)).filter((ms) => !Number.isNaN(ms)),
  );
  const startedBeforePage = (turn: Turn): boolean => Date.parse(turn.ts) < pageStartMs;

  const historyTurns: Turn[] = [];
  const liveTurns: Turn[] = [];
  for (const turn of prior.model.turns) {
    if (incomingTurnIds.has(turn.id)) continue;
    const surviving = turn.entryIds.filter((id) => entries[id]);
    if (surviving.length === 0) continue;
    const kept = surviving.length === turn.entryIds.length ? turn : { ...turn, entryIds: surviving };
    if (surviving.some((id) => liveHeldIds.has(id)) && !startedBeforePage(turn)) liveTurns.push(kept);
    else historyTurns.push(kept);
  }
  const pageTurns: Turn[] = incoming.turns.map((turn) => {
    const extra = keptByTurn.get(turn.id);
    if (!extra) return turn;
    // The page's entries first in the page's order, kept live entries after in
    // their prior relative order — the live tail is by construction the newer.
    const keptInPriorOrder = (prior.model.turns.find((t) => t.id === turn.id)?.entryIds ?? [])
      .filter((id) => extra.includes(id));
    const orderedExtra = keptInPriorOrder.length === extra.length ? keptInPriorOrder : extra;
    return { ...turn, entryIds: [...turn.entryIds, ...orderedExtra] };
  });
  const turns: Turn[] = [...historyTurns, ...pageTurns, ...liveTurns];

  // Source groups accumulate: the page reports groups for ITS window only, and the
  // history turns kept above still reference theirs. Dropping the prior map would
  // strip the sources badge from every row that scrolled out of the page window.
  const sourceGroups =
    prior.model.sourceGroups || incoming.sourceGroups
      ? { ...(prior.model.sourceGroups ?? {}), ...(incoming.sourceGroups ?? {}) }
      : undefined;
  const model: TurnModel = { ...incoming, turns, entries, ...(sourceGroups ? { sourceGroups } : {}) };

  // --- Stream-space bookkeeping carries over; page ids never enter it. ---
  const turnIndex = new Map<string, number>();
  turns.forEach((t, i) => turnIndex.set(t.id, i));
  const entryEventIds = new Map<string, Set<number>>();
  for (const id of Object.keys(entries)) {
    const priorSet = prior.entryEventIds.get(id);
    if (priorSet && priorSet.size > 0) entryEventIds.set(id, new Set(priorSet));
  }
  const seenEventIds = new Set(prior.seenEventIds);

  return { sessionId: incoming.sessionId, model, turnIndex, entryEventIds, seenEventIds };
}

/** Reseed a tail from a merged model while KEEPING the fold history of every entry
 *  that survived. `prependOlder` used to reseed with `initTailState` alone, which
 *  collapsed a live entry's folded event-id set to its single latest id — so a
 *  Last-Event-ID replay after paging up could re-fold text the entry already held. */
export function reseedTailKeepingFoldHistory(
  sessionId: string,
  model: TurnModel,
  prior: TailState | undefined,
): TailState {
  const tail = initTailState(sessionId, model);
  if (!prior) return tail;
  for (const [id, ids] of prior.entryEventIds) {
    if (!(id in model.entries) || ids.size === 0) continue;
    tail.entryEventIds.set(id, new Set(ids));
  }
  for (const n of prior.seenEventIds) tail.seenEventIds.add(n);
  return tail;
}

function eventIdOf(ev: WireEvent): number {
  if (typeof ev.data.event_id === 'number') return ev.data.event_id;
  if (ev.id) return Number(ev.id) || 0;
  return 0;
}

function sourceOf(data: WireEventData): 'harness' | 'otel' {
  return data.extensions?.source === 'otel' ? 'otel' : 'harness';
}

// The entry kind + role an event maps to. Mirrors bridge-ui's rowKindOf/actorFor,
// projected onto the chat-core Entry vocabulary.
//
// Exported because the activity fold (store/activity.ts) needs exactly this
// discrimination — is a stream delta thinking or plain text, is a block a thinking
// block — and a second copy of it would drift. Note it deliberately collapses
// `tool_call` and `tool_result` onto one kind; a caller that has to tell those two
// apart still switches on `ev.type` itself.
export function kindOf(ev: WireEvent): EntryKind {
  switch (ev.type) {
    case 'user_message':
      return 'text';
    case 'stream': {
      const dt = ev.data.stream?.delta?.type;
      if (dt === 'thinking_delta') return 'thinking';
      return 'text';
    }
    case 'block': {
      const bt = ev.data.block?.block?.type;
      if (bt === 'thinking') return 'thinking';
      return 'text';
    }
    case 'thinking':
      return 'thinking';
    case 'tool_call':
    case 'tool_result':
      return 'tool_call';
    case 'result':
      return 'result';
    case 'error':
      return 'error';
    case 'system':
      return 'system';
    case 'session_file':
      return 'file';
    case 'session_action':
      return 'action';
    case 'session_state':
    case 'session_info':
      return 'meta';
    default:
      return 'meta';
  }
}

function roleOf(ev: WireEvent, kind: EntryKind): Role {
  if (ev.type === 'user_message') return 'user';
  // A shared file is said by whoever shared it — log-store's classify does the same.
  if (ev.type === 'session_file') return ev.data.session_file?.shared_by === 'agent' ? 'assistant' : 'user';
  // The agent offers a button; each later step of its run is a record of what happened.
  // log-store's classify does the same.
  if (ev.type === 'session_action') return ev.data.session_action?.state === 'offered' ? 'assistant' : 'system';
  if (ev.type === 'tool_call' || ev.type === 'tool_result') return 'tool';
  if (ev.type === 'system' || ev.type === 'session_state' || ev.type === 'session_info') {
    return 'system';
  }
  void kind;
  return 'assistant';
}

// Grouping key: the stable Entry id. tool_call/tool_result pair by tool_id;
// user prompts and assistant content group by message_id + kind; anything
// without a message_id stands alone keyed by its event id.
//
// A tool event with no tool_id cannot be paired with anything, and falling
// through to the `evt_<id>` key below is what strands it: the call becomes one
// entry and its result another, so the call row renders "running" forever with
// no result that can ever reach it. Some events legitimately arrive that way —
// OTel-derived tool stand-ins carry no tool id by design — so this is not rare.
//
// Pairing them by (message, tool name) instead is wrong in the other direction:
// two calls to the same tool in one message would collapse onto each other and
// one result would overwrite the other. So the honest key is still per-event,
// and the fix belongs at the render edge, which must not show a permanent
// spinner for a row it knows can never be resolved. `unpairable` marks that.
function groupKeyFor(ev: WireEvent, kind: EntryKind): string {
  if (ev.type === 'tool_call' || ev.type === 'tool_result') {
    const toolId = ev.data.tool_call?.tool_id || ev.data.tool_result?.tool_id;
    if (toolId) return `tool_${toolId}`;
  }
  const msgId = ev.data.message_id;
  if (msgId) {
    if (ev.type === 'user_message') return `${msgId}_user`;
    return `${msgId}_${kind}`;
  }
  return `evt_${eventIdOf(ev)}`;
}

/** The turn an event belongs to. An event with NO turn_id attaches to the CURRENT
 *  (last) turn when one exists — the same carry-forward rule log-store's
 *  `buildTurns` applies — and only opens a synthetic solo turn when nothing is
 *  open yet. The old rule (always solo) diverged from the materializer AND broke
 *  the live view: one turn-less `system` event arrives ~100ms into every real
 *  turn (measured, br_1787619223437999622), its solo turn sat AFTER the real
 *  turn forever, and everything keyed on "the last turn" — the streaming
 *  indicator, the live narration aside, the provisional-narration rule — pointed
 *  at a one-row bookkeeping turn instead of the turn actually running. */
function turnKeyFor(ev: WireEvent, entryId: string, currentTurnId: string | null): string {
  return ev.data.turn_id || currentTurnId || `solo_${entryId}`;
}

// Fold an event's payload onto an entry (accumulating streamed text). Returns a
// new Entry; never mutates `prev`.
function applyPayload(prev: Entry, ev: WireEvent): Entry {
  const next: Entry = { ...prev, eventId: Math.max(prev.eventId, eventIdOf(ev)) };
  const raw = ev.data;
  switch (ev.type) {
    case 'user_message':
      next.text = raw.result?.text ?? prev.text ?? '';
      break;
    // A harness streams a block's text as deltas and then sends the same block
    // whole. Both land on one entry (same message, same kind), so adding the
    // whole block after its deltas showed the reply twice until a reload read
    // the page. `wholeBlocksText` is the text as of the last whole block: a delta
    // adds to `text`, and a whole block replaces what the deltas built since.
    case 'stream': {
      const d = raw.stream?.delta;
      const piece = d?.type === 'text_delta' ? d.text : d?.type === 'thinking_delta' ? d.thinking : undefined;
      if (piece !== undefined) {
        next.wholeBlocksText = prev.wholeBlocksText ?? prev.text ?? '';
        next.text = (prev.text || '') + piece;
      }
      break;
    }
    case 'block': {
      const b = raw.block?.block;
      const whole = b?.type === 'text' && b.text_block ? b.text_block.text
        : b?.type === 'thinking' && b.thinking_block ? b.thinking_block.text
        : undefined;
      if (whole !== undefined) {
        next.text = (prev.wholeBlocksText ?? prev.text ?? '') + whole;
        next.wholeBlocksText = next.text;
      }
      break;
    }
    case 'thinking':
      next.text = (prev.text || '') + (raw.thinking?.text || '');
      break;
    case 'tool_call':
      next.toolName = raw.tool_call?.name ?? prev.toolName;
      next.toolInput = raw.tool_call?.input ?? prev.toolInput;
      // Promoted off `raw` so the two paths agree. log-store carries `toolId` on the
      // materialized page; without the same field here a live-folded entry and the
      // page copy of the SAME tool call would disagree about its id, and pairing
      // would break on exactly the sessions being watched stream.
      next.toolId = raw.tool_call?.tool_id ?? prev.toolId;
      break;
    case 'tool_result':
      next.toolName = raw.tool_result?.name ?? prev.toolName;
      next.toolResult = raw.tool_result?.output ?? prev.toolResult;
      next.toolId = raw.tool_result?.tool_id ?? prev.toolId;
      if (raw.tool_result?.is_error !== undefined) next.toolError = raw.tool_result.is_error;
      {
        const images = liveToolResultImages(raw.tool_result?.content);
        if (images.length > 0) next.toolResultImages = images;
      }
      break;
    case 'session_file':
      if (raw.session_file) next.sessionFile = raw.session_file;
      break;
    case 'session_action':
      if (raw.session_action) next.sessionAction = raw.session_action;
      break;
    case 'result':
      next.text = raw.result?.text || prev.text;
      break;
    case 'error':
      next.text = raw.error?.message || prev.text || 'error';
      // Carry the canonical ErrorEvent fields so the edge can style the chip and
      // the terminal-state reconcile can spot TURN_IDLE_TIMEOUT / PROCESS_DIED.
      if (raw.error?.code !== undefined) next.code = raw.error.code;
      if (raw.error?.retryable !== undefined) next.retryable = raw.error.retryable;
      if (raw.error?.status_code !== undefined) next.statusCode = raw.error.status_code;
      break;
    case 'system':
      next.text = raw.system?.message ?? prev.text;
      if (raw.system?.subtype !== undefined) next.subtype = raw.system.subtype;
      // Subagent correlators and outcome. These were reachable only by digging
      // through the untyped `raw` blob, so the timeline read `task_id` from it
      // and nothing read the status at all — which is why a finished subagent
      // never reported. Carry them onto the entry like every other canonical
      // field, and let the edge render from the model.
      if (raw.system?.task_id !== undefined) next.taskId = raw.system.task_id;
      if (raw.system?.tool_use_id !== undefined) next.toolUseId = raw.system.tool_use_id;
      if (raw.system?.task_status !== undefined) next.taskStatus = raw.system.task_status;
      if (raw.system?.task_summary !== undefined) next.taskSummary = raw.system.task_summary;
      if (raw.system?.task_output_file !== undefined) {
        next.taskOutputFile = raw.system.task_output_file;
      }
      if (raw.system?.held_message_id !== undefined) next.heldMessageId = raw.system.held_message_id;
      if (raw.system?.task_type !== undefined) next.taskType = raw.system.task_type;
      if (raw.system?.subagent_type !== undefined) {
        next.subagentType = raw.system.subagent_type;
      }
      if (raw.system?.subagent_session_id !== undefined) {
        next.subagentSessionId = raw.system.subagent_session_id;
      }
      if (raw.system?.last_tool_name !== undefined) {
        next.lastToolName = raw.system.last_tool_name;
      }
      // task_started names the subagent; its description is the only human
      // label the task header ever gets.
      if (!next.text && raw.system?.description) next.text = raw.system.description;
      break;
    default:
      break;
  }
  // The canonical message id, carried as a field on every entry that has one —
  // including TOOL entries, whose key `tool_<toolId>` does not encode it and
  // which the per-message view must be able to ask "did this message tool?" of.
  // For a tool entry the CALL's message is the one that question is about (the
  // result arrives under a different message id), so the call's id wins; every
  // other entry's folded events share one id by construction (groupKeyFor).
  if (ev.type === 'tool_call' && raw.message_id) {
    next.messageId = raw.message_id;
  } else if (!next.messageId && raw.message_id) {
    next.messageId = raw.message_id;
  }
  // Recovered-assistant provenance rides on extensions (block events); never gates
  // visibility — it's a presentation marker only.
  if (raw.extensions?.recovered === true) next.recovered = true;
  // Whose work this is. Carried on every event type, not just one kind, so it is
  // read here rather than in the switch above. It reached the browser on ~12,000
  // events and no consumer looked at it, which is how another session's rows
  // ended up rendering as this one's.
  if (raw.harness_parent_id) next.harnessParentId = raw.harness_parent_id;
  // Carried on every event type rather than in the switch, same as the two above,
  // and for the same reason the page carries them: so nothing has to read `raw`.
  if (raw.client_request_id) next.clientRequestId = raw.client_request_id;
  // Who wrote a user message, as the page carries it (log-store's promoted
  // senderPrincipalId / senderDisplayName).
  if (raw.sender_principal_id) next.senderPrincipalId = raw.sender_principal_id;
  if (raw.sender_display_name) next.senderDisplayName = raw.sender_display_name;
  if (raw.type) next.eventType = raw.type;
  next.raw = raw;
  return next;
}

/**
 * Apply one live event to a tail state, returning a NEW state (pure; the input
 * is never mutated). O(1): entry + turn lookups are Map/record indexed, and a
 * repeated `event_id` is a no-op — so replaying from Last-Event-ID converges.
 * Re-runs the OTel annotator so the collapsed/raw views stay correct.
 */
/** True when a stream frame re-delivers content the model already holds as a
 *  entry its own frames did not build — the cross-space replay the numeric seen-set cannot catch
 *  (the frame's id is a bridge row, the page's eventIds are log-store rows).
 *  Happens on every cold open: the page is fetched, then the SSE connects with
 *  no cursor and the server replays the current turn — the same events the page
 *  just reported. The match is the shared content identity, against
 *  an entry with no folded stream ids, whether the page's own (`e_<id>`) or a
 *  live-keyed one (`${messageId}_${kind}`) that the cache handed back on boot. A
 *  live entry this tail built itself is reachable by its own key and is never
 *  matched here: same messageId+role+kind, with the text CONTAINED for blocks (a
 *  fresh second block of the same message legitimately folds), same tool_id for tools.
 *  Bookkeeping frames (no message id, no tool id) always fold — their
 *  materialized twins are hidden, so nothing doubles on screen. */
function heldCopyExists(
  state: TailState,
  ev: WireEvent,
  kind: EntryKind,
  role: Role,
): boolean {
  const data = ev.data;
  // ⚠️ The frame-id map on purpose, NOT `Entry.origin`. The question here is whether
  // THIS tail's frames built the entry. A live row the cache handed back is `origin:
  // 'live'` and still must count as already held, or the replayed frame appends a
  // second copy of the answer (hydratedTailReplay.test.ts). The merge asks the other
  // question — where did the entry come from — and that one the stamp answers.
  const heldBeforeThisTailsFrames = (e: Entry) => (state.entryEventIds.get(e.id)?.size ?? 0) === 0;

  if (ev.type === 'tool_call' || ev.type === 'tool_result') {
    const toolId = data.tool_call?.tool_id || data.tool_result?.tool_id;
    if (!toolId) return false;
    const wantKind = ev.type;
    for (const e of Object.values(state.model.entries)) {
      if (e.kind !== wantKind || !heldBeforeThisTailsFrames(e)) continue;
      const raw = e.raw as
        | { tool_call?: { tool_id?: string }; tool_result?: { tool_id?: string } }
        | undefined;
      if ((raw?.tool_call?.tool_id || raw?.tool_result?.tool_id) === toolId) return true;
    }
    return false;
  }

  const msgId = data.message_id;
  if (!msgId) return false;
  let text: string | undefined;
  if (ev.type === 'user_message') text = data.result?.text ?? '';
  else if (ev.type === 'result') text = undefined; // presence of the unit suffices
  else if (ev.type === 'block') {
    const b = data.block?.block;
    text = b?.text_block?.text ?? b?.thinking_block?.text ?? '';
  } else if (ev.type === 'thinking') text = data.thinking?.text ?? '';
  else return false; // stream deltas and the rest always fold

  for (const e of Object.values(state.model.entries)) {
    if (e.messageId !== msgId || e.role !== role || e.kind !== kind) continue;
    if (!heldBeforeThisTailsFrames(e)) continue;
    if (text === undefined) return true;
    if (ev.type === 'user_message') {
      if (normalizeText(e.text ?? '') === normalizeText(text)) return true;
      continue;
    }
    if (text === '' || (e.text ?? '').includes(text)) return true;
  }
  return false;
}

export function applyEvent(state: TailState, ev: WireEvent): TailState {
  return foldEvent(state, ev, true);
}

/**
 * Re-annotate a tail's entries in one pass.
 *
 * Split out of the fold so a BATCH of frames can annotate once instead of once per
 * frame. `annotateOTelDuplicates` rebuilds every entry (`entries.map(e => ({...e}))`)
 * and runs over the WHOLE model, which on a cold-loaded session is a thousand entries —
 * so per-frame it is O(frames x entries), and a session open replays hundreds of frames.
 *
 * Safe to defer because the fold never READS the annotations it writes: a new entry is
 * created with the `duplicate: false, primary: true` default and nothing on the fold
 * path consults those fields. An intermediate state therefore carries stale annotations
 * that nothing can observe, and this pass recomputes all three from scratch.
 */
export function annotateTail(state: TailState): TailState {
  return {
    ...state,
    model: { ...state.model, entries: annotatedRecord(state.model.entries) },
  };
}

function annotatedRecord(entries: Record<string, Entry>): Record<string, Entry> {
  // A materialized entry is the page's own — judged by the entry's stamp, which
  // survives a cache round trip, not by this tail's frame-id map, which does not.
  const isMaterialized = (e: Entry) => !isLiveEntry(e);
  const annotated = annotateOTelDuplicates(Object.values(entries));
  const out: Record<string, Entry> = {};
  for (const a of annotated) {
    const held = entries[a.id];
    if (!held) {
      out[a.id] = a;
      continue;
    }
    // Two rules on top of the annotator's fresh verdict:
    //  - a MATERIALIZED entry's server-side `duplicate` stands. log-store decides
    //    which streamed blocks a result superseded, and that verdict is not
    //    something the client can re-derive from text pairing. This pass only
    //    ever ADDS pairs; it never clears a mark the server made. (Before this,
    //    a re-annotation after a merge un-hid every superseded block the page
    //    carried, which is what the carve-out test in setTurnsMerge caught.)
    //  - an entry whose three annotation fields come out unchanged keeps its
    //    OBJECT. `setTurns` promises row memos that unchanged content keeps its
    //    identity, and the annotator's `{...e}` rebuild broke that promise for
    //    every entry on every repair.
    const serverDuplicate = isMaterialized(held) && held.duplicate === true;
    const duplicate = a.duplicate || serverDuplicate;
    const primary = duplicate ? false : a.primary;
    const groupId = a.groupId ?? (isMaterialized(held) ? held.groupId : undefined);
    if (held.duplicate === duplicate && held.primary === primary && held.groupId === groupId) {
      out[a.id] = held;
      continue;
    }
    out[a.id] = { ...a, duplicate, primary, groupId };
  }
  return out;
}

/**
 * Fold one event onto a tail WITHOUT re-annotating OTel duplicates.
 *
 * For a caller folding a batch: annotate once at the end with `annotateTail` instead of
 * once per frame. The returned state carries stale `duplicate`/`primary`/`groupId`
 * until that happens, which is safe only because the fold never reads them — see
 * `annotateTail`. A caller that renders from the result before annotating gets the
 * PREVIOUS pass's grouping, so this is not the function to reach for by default;
 * `applyEvent` is.
 */
export function foldEventWithoutAnnotating(state: TailState, ev: WireEvent): TailState {
  return foldEvent(state, ev, false);
}

function foldEvent(state: TailState, ev: WireEvent, annotate: boolean): TailState {
  const evId = eventIdOf(ev);
  if (evId && state.seenEventIds.has(evId)) {
    return state; // idempotent — already folded in.
  }

  const kind = kindOf(ev);
  const role = roleOf(ev, kind);
  const entryId = groupKeyFor(ev, kind);
  const ts = ev.data.timestamp || new Date().toISOString();

  const entries: Record<string, Entry> = { ...state.model.entries };
  const turnIndex = new Map(state.turnIndex);
  const entryEventIds = new Map(state.entryEventIds);
  const seenEventIds = new Set(state.seenEventIds);
  let turns = state.model.turns;

  const existing = entries[entryId];
  // A same-key entry that was NOT built by this tail's own frames (no folded stream
  // ids) is one the cache handed back: a live entry that survived a page merge and
  // was written to IndexedDB. A frame re-delivered onto it is a replay, not new
  // text, and `applyPayload` would APPEND it — one more copy of the answer per
  // reload, which is what was measured on br_1788973449319671731 (2026-09-10).
  const existingIsHydrated =
    existing !== undefined && (state.entryEventIds.get(entryId)?.size ?? 0) === 0;
  if ((!existing || existingIsHydrated) && heldCopyExists(state, ev, kind, role)) {
    // A cross-space replay: fold nothing, but record the frame id so the check
    // is O(1) next time and a later literal replay short-circuits at the top.
    if (!evId) return state;
    const seenOnly = new Set(state.seenEventIds);
    seenOnly.add(evId);
    return { ...state, seenEventIds: seenOnly };
  }
  let entry: Entry;
  if (existing) {
    entry = applyPayload(existing, ev);
  } else {
    const fresh: Entry = {
      id: entryId,
      turnId: turnKeyFor(ev, entryId, state.model.turns[state.model.turns.length - 1]?.id ?? null),
      role,
      kind,
      source: sourceOf(ev.data),
      eventId: evId,
      ts,
      origin: 'live',
      duplicate: false,
      primary: true,
      // A tool event with no tool_id is keyed by event id, so nothing can ever
      // join it to its counterpart. Recorded here, where the key was chosen,
      // rather than re-derived at the edge.
      unpairable:
        (ev.type === 'tool_call' || ev.type === 'tool_result') &&
        !(ev.data.tool_call?.tool_id || ev.data.tool_result?.tool_id),
    };
    entry = applyPayload(fresh, ev);
  }
  entries[entryId] = entry;

  // Track contributing event ids for this entry + globally, for dedup.
  const ids = new Set(entryEventIds.get(entryId) ?? []);
  if (evId) ids.add(evId);
  entryEventIds.set(entryId, ids);
  if (evId) seenEventIds.add(evId);

  // Attach the entry to its turn (creating the turn if new).
  const turnId = entry.turnId;
  const tIdx = turnIndex.get(turnId);
  if (tIdx === undefined) {
    const turn: Turn = { id: turnId, role: entry.role, ts, entryIds: [entryId] };
    turns = [...turns, turn];
    turnIndex.set(turnId, turns.length - 1);
  } else if (!existing) {
    const turn = turns[tIdx];
    if (turn && !turn.entryIds.includes(entryId)) {
      // APPEND, never sort: the stream arrives in order, and after a merge the
      // turn also holds materialized entries whose eventIds are log-store ids —
      // a different numbering — so sorting the mix by eventId put this (newest)
      // entry FIRST. That was the reported "narration arrives out of order".
      const nextTurn: Turn = {
        ...turn,
        entryIds: [...turn.entryIds, entryId],
      };
      turns = turns.slice();
      turns[tIdx] = nextTurn;
    }
  }

  // Re-annotate OTel duplicates so the collapsed and raw views stay correct as a late
  // OTel copy arrives. Non-destructive: it only tags duplicate/primary/groupId.
  //
  // ⚠️ NOT "across the small live tail", which is what this said for as long as the tail
  // was live-only. Once `mergeMaterializedPage` folds a fetched page in, `entries` is
  // the whole model — a thousand of them on a real session — and this rebuilds every one.
  // Skipped for a batched fold, which annotates once at the end instead; see
  // `annotateTail`.
  const annotatedEntries = annotate ? annotatedRecord(entries) : entries;

  const maxEventId = Math.max(state.model.validator.maxEventId, evId);
  // Spread the prior model rather than re-listing its fields. This used to be an
  // explicit five-field literal, and the sixth field TurnModel grew — `aggregates`, the
  // server's cost/context roll-up — was therefore ERASED by every event folded onto the
  // tail. That is the chat header flicker: `GET /turns` delivered the roll-up, the
  // cost chip and context strip painted, and the very next streamed token dropped it
  // again, taking 14px of header height with it on every blink.
  //
  // log-store computes `aggregates` as last-value-wins over the RETURNED PAGE and
  // documents the consequence explicitly (turnmodel.go): "If the spend/context events
  // fall outside the page … the whole struct may be nil — the client falls back to its
  // live-tail values." A wire event carries no spend total and no context figure, so it
  // has nothing to replace them WITH; dropping them reports "this session has no cost
  // data" about a session that does. Carrying them forward is that documented fallback.
  //
  // Spreading is the point, not a shortcut: an explicit field list is a standing promise
  // to remember to edit this literal every time the shape grows, and that promise has
  // already been broken once. Whatever field TurnModel gains next survives the tail by
  // default and has to be dropped ON PURPOSE to be dropped at all.
  const model: TurnModel = {
    ...state.model,
    sessionId: state.model.sessionId || state.sessionId,
    turns,
    entries: annotatedEntries,
    validator: {
      maxEventId,
      eventCount: seenEventIds.size,
      updatedAt: ts,
    },
  };

  return { sessionId: state.sessionId, model, turnIndex, entryEventIds, seenEventIds };
}

/**
 * Fold a batch of events in order, annotating ONCE at the end.
 *
 * Not a convenience wrapper — the single annotation is the point. Folding a replayed
 * turn frame-by-frame ran `annotateOTelDuplicates` over the entire model per frame,
 * which is O(frames x entries) for a result that only the last pass can be right about
 * anyway. Every frame is still folded individually, in order, through the same reducer.
 */
export function applyEvents(state: TailState, events: WireEvent[]): TailState {
  let s = state;
  for (const ev of events) s = foldEvent(s, ev, false);
  return s === state ? state : annotateTail(s);
}
