import { afterEach, describe, expect, it, vi } from 'vitest';

// Frames arrive off `for await`, and every iteration of that loop is a separate task.
// React cannot batch state updates across an await boundary, so applying each frame as
// it arrived meant ONE RE-RENDER PER FRAME — and opening a session replays its whole
// current turn, which is hundreds of frames.
//
// Measured 2026-08-26 on the real dashboard, eight cold session opens: 12,751ms of
// main-thread blocking. Cutting the per-session SSE off entirely took the same eight
// opens to 261ms, so 98% of the cost was this. Batching the frames into one task took
// it to 1,861ms.
//
// It took three experiments to find because the obvious suspects were innocent, and
// these cases exist so nobody has to run them again:
//   - clipping tool payloads   6.17MB -> 4.02MB  moved the total 3%
//   - removing every tool ENTRY 6.17MB -> 0.54MB, 1043 entries -> 118, moved it 0%
//   - blocking the SSE                            moved it 98%
//
// So the assertion is on the NUMBER OF STORE NOTIFICATIONS, not on bytes, not on entry
// count, and not on elapsed time. Notifications are what React turns into renders, they
// are exactly what regressed, and unlike a stopwatch they do not flake on a busy box.

const connectSessionSSE = vi.fn();
const connectListSSE = vi.fn();

vi.mock('../src/sync/sse.js', () => ({
  connectSessionSSE: (...args: unknown[]) => connectSessionSSE(...args),
  connectListSSE: (...args: unknown[]) => connectListSSE(...args),
}));

const { SyncEngine } = await import('../src/sync/SyncEngine.js');
const { createChatStore } = await import('../src/store/ChatStore.js');
type TurnModel = import('../src/net/types.js').TurnModel;
type WireEvent = import('../src/net/wireEvents.js').WireEvent;
type ApiClient = import('../src/net/ApiClient.js').ApiClient;
type SessionCache = import('../src/cache/SessionCache.js').SessionCache;

const SID = 'br_active';

function emptyModel(sessionId = SID): TurnModel {
  return {
    sessionId,
    turns: [],
    entries: {},
    validator: { maxEventId: 0, eventCount: 0, updatedAt: '' },
    more: false,
  };
}

/** One streamed assistant text frame, with a fresh id so nothing rides the dedup. */
function frame(eventId: number): WireEvent {
  return {
    id: String(eventId),
    type: 'stream',
    data: {
      type: 'stream',
      event_id: eventId,
      turn_id: 't1',
      stream: { delta: { type: 'text_delta', text: `tok${eventId} ` } },
    },
  };
}

async function* noEvents(): AsyncGenerator<never> {
  return;
}

/**
 * A stream that yields `count` frames, each after a real await — which is what makes
 * this a test of BATCHING rather than of a loop. Yielding them synchronously would be
 * batched by any implementation, including the broken one.
 */
function streamOf(count: number) {
  return async function* (): AsyncGenerator<WireEvent> {
    for (let i = 1; i <= count; i++) {
      await Promise.resolve();
      yield frame(i);
    }
  };
}

function engine() {
  const store = createChatStore();
  store.getState().actions.setActive(SID);
  store.getState().actions.setTurns(SID, emptyModel());
  const api = {
    fetchFor: () => vi.fn(),
    basePath: '/api/bridge',
    getValidators: vi.fn(async () => ({})),
    getMessages: vi.fn(async () => ({ model: emptyModel() })),
    listFolders: vi.fn(async () => []),
  } as unknown as ApiClient;
  const cache = {
    isEnabled: false,
    putTurns: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}),
  } as unknown as SessionCache;
  return { store, cache, engine: new SyncEngine({ store, api, cache }) };
}

/** Let the stream drain and the batch flush (a `setTimeout(0)`) run. */
async function settle(): Promise<void> {
  await new Promise((r) => setTimeout(r, 20));
}

let running: { stop: () => void } | null = null;
afterEach(() => {
  running?.stop();
  running = null;
  connectSessionSSE.mockReset();
  connectListSSE.mockReset();
});

describe('streamed frames are applied in batches, not one render each', () => {
  it('turns a replayed turn into ONE store notification, not one per frame', async () => {
    connectSessionSSE.mockImplementation(streamOf(50));
    connectListSSE.mockImplementation(() => noEvents());
    const { store, engine: e } = engine();

    let notifications = 0;
    const unsubscribe = store.subscribe((s, prev) => {
      // Only transcript changes count — the engine also writes connState and activity,
      // and those are not what multiplied.
      if (s.turnsBySession !== prev.turnsBySession) notifications++;
    });

    running = e;
    e.start();
    await settle();
    unsubscribe();

    expect(notifications).toBeGreaterThan(0); // the frames really were applied
    expect(notifications).toBeLessThan(5); // ...in a handful of batches, not 50
  });

  it('applies every frame — batching must not drop content', async () => {
    // The failure mode a naive batcher has: coalescing by replacing rather than
    // accumulating, so only the last frame of each batch survives. The text is the
    // proof, and it has to be ALL of it.
    connectSessionSSE.mockImplementation(streamOf(20));
    connectListSSE.mockImplementation(() => noEvents());
    const { store, engine: e } = engine();

    running = e;
    e.start();
    await settle();

    const text = Object.values(store.getState().turnsBySession.get(SID)?.entries ?? {})
      .map((entry) => entry.text ?? '')
      .join('');
    for (let i = 1; i <= 20; i++) {
      expect(text).toContain(`tok${i} `);
    }
  });

  it('keeps the frames in order', async () => {
    connectSessionSSE.mockImplementation(streamOf(12));
    connectListSSE.mockImplementation(() => noEvents());
    const { store, engine: e } = engine();

    running = e;
    e.start();
    await settle();

    const text = Object.values(store.getState().turnsBySession.get(SID)?.entries ?? {})
      .map((entry) => entry.text ?? '')
      .join('');
    expect(text.indexOf('tok1 ')).toBeLessThan(text.indexOf('tok12 '));
    expect(text.indexOf('tok2 ')).toBeLessThan(text.indexOf('tok3 '));
  });

  it('writes the cache once for the batch rather than once per frame', async () => {
    // The other multiplier on this path. IndexedDB structured-clones its argument on
    // the main thread, so a write per frame was 2,449 full-model clones across eight
    // opens before the coalescing landed — see cacheWriteCoalescing.test.ts.
    connectSessionSSE.mockImplementation(streamOf(40));
    connectListSSE.mockImplementation(() => noEvents());
    const { cache, engine: e } = engine();

    running = e;
    e.start();
    await settle();

    expect((cache.scheduleTurnsWrite as unknown as { mock: { calls: unknown[] } }).mock.calls.length)
      .toBeLessThan(5);
  });

  it('flushes what is buffered when the engine stops', async () => {
    // Frames sitting in the buffer when a stream ends belong on screen. Without the
    // flush the tail of every turn would arrive only when the NEXT frame did — which,
    // at the end of a turn, is never.
    connectSessionSSE.mockImplementation(streamOf(5));
    connectListSSE.mockImplementation(() => noEvents());
    const { store, engine: e } = engine();

    running = e;
    e.start();
    await settle();

    const text = Object.values(store.getState().turnsBySession.get(SID)?.entries ?? {})
      .map((entry) => entry.text ?? '')
      .join('');
    expect(text).toContain('tok5 ');
  });
});
