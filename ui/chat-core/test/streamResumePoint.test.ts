import { afterEach, describe, expect, it, vi } from 'vitest';

// The server replays the ENTIRE current turn to a client that connects without a
// `Last-Event-ID`, because it has no way to know what that client already holds.
// Measured 2026-08-26 on three real sessions: 88, 177 and 360 frames, up to 1,154 KB —
// every byte of it content the page had delivered milliseconds earlier. Handed a resume
// point, the same connects replay 0 frames and 0 bytes.
//
// Every messages page carries that resume point (`MessagesResponse.stream.head`), and
// ⛔ the client does NOT use it: sending it doubled narration in the UI and was withdrawn
// on 2026-08-27, and the plumbing that stored it was deleted on 2026-09-16.
// `SyncEngine.streamCursors` carries the account and what a second attempt must fix.
//
// These cases pin what stands: the stream opens without a page-derived resume point,
// and — the one that would silently lose transcript — never with a log-store id.

const connectSessionSSE = vi.fn();
const connectListSSE = vi.fn();

vi.mock('../src/sync/sse.js', () => ({
  connectSessionSSE: (...args: unknown[]) => connectSessionSSE(...args),
  connectListSSE: (...args: unknown[]) => connectListSSE(...args),
}));

const { SyncEngine } = await import('../src/sync/SyncEngine.js');
const { createChatStore } = await import('../src/store/ChatStore.js');
type TurnModel = import('../src/net/types.js').TurnModel;
type ApiClient = import('../src/net/ApiClient.js').ApiClient;
type SessionCache = import('../src/cache/SessionCache.js').SessionCache;

const SID = 'br_active';

/** A model whose LOG-STORE ids are deliberately huge, so a resume point taken from the
 *  wrong id space is unmistakable in an assertion. */
function model(maxEventId: number): TurnModel {
  return {
    sessionId: SID,
    turns: [],
    entries: {},
    validator: { maxEventId, eventCount: maxEventId, updatedAt: '2026-08-26T00:00:00Z' },
    more: false,
  };
}

async function* noEvents(): AsyncGenerator<never> {
  return;
}

function engine() {
  const store = createChatStore();
  const api = {
    fetchFor: () => vi.fn(),
    basePath: '/api/bridge',
    getValidators: vi.fn(async () => ({})),
    getMessages: vi.fn(async () => ({ model: model(0) })),
    listFolders: vi.fn(async () => []),
  } as unknown as ApiClient;
  const cache = {
    isEnabled: false,
    putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}),
  } as unknown as SessionCache;
  return { store, engine: new SyncEngine({ store, api, cache }) };
}

/** The `Last-Event-ID` the stream was opened with (4th argument). */
function resumePointSent(): unknown {
  return connectSessionSSE.mock.calls.at(-1)?.[3];
}

let running: { stop: () => void } | null = null;
afterEach(() => {
  running?.stop();
  running = null;
  connectSessionSSE.mockReset();
  connectListSSE.mockReset();
});

describe('the page tells the stream where to resume', () => {
  it('does NOT yet open the stream with it — see the withdrawal note', async () => {
    // ⛔ The page's resume point is deliberately not sent. Sending it doubled
    // narration in the UI: the page can contain events ABOVE the head (written while the
    // server flushed), the stream re-delivers those, and page-vs-live entries never
    // collide by id — overlap is reconciled by CONTENT in `mergeMaterializedPage`, which
    // only runs when a page lands OVER a live tail. Resuming from a page inverts that
    // order. `SyncEngine.streamCursors` carries the full account.
    //
    // This case exists so re-enabling it is a deliberate act with a red test to answer.
    connectSessionSSE.mockImplementation(() => noEvents());
    connectListSSE.mockImplementation(() => noEvents());
    const { store, engine: e } = engine();
    store.getState().actions.setTurns(SID, model(2094222));
    store.getState().actions.setActive(SID);

    running = e;
    e.start();
    await new Promise((r) => setTimeout(r, 10));

    expect(resumePointSent()).toBeUndefined();
  });

  it('NEVER sends the log-store id, whatever it does send', async () => {
    // The failure this codebase has already paid for, and it outlives the withdrawal
    // above: `validator.maxEventId` is a log-store row id and the server parses
    // `Last-Event-ID` in its own space. Sending the log-store number — numerically far
    // ahead — made the server replay nothing, so every reconnect silently missed the
    // events between the page and the stream. Whatever this function returns, it must
    // never be that number.
    connectSessionSSE.mockImplementation(() => noEvents());
    connectListSSE.mockImplementation(() => noEvents());
    const { store, engine: e } = engine();
    store.getState().actions.setTurns(SID, model(2094222));
    store.getState().actions.setActive(SID);

    running = e;
    e.start();
    await new Promise((r) => setTimeout(r, 10));

    expect(resumePointSent()).not.toBe('2094222');
  });
});
