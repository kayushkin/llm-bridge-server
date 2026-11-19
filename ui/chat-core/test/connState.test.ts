import { describe, expect, it, vi } from 'vitest';
import { SyncEngine } from '../src/sync/SyncEngine.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { ApiClient } from '../src/net/ApiClient.js';
import type { SessionCache } from '../src/cache/SessionCache.js';

// `connState` is the only signal that separates "still connecting" from "you have no
// sessions", and the only one that says updates have stopped arriving. `useConnState`
// is a bare `useStore(store, s => s.connState)` selector, so what is worth testing is
// not the selector — it is the state machine underneath it, and specifically the one
// transition a consumer is most likely to get wrong: a DROPPED stream goes back to
// 'connecting', never to 'closed'. A UI that tests `!== 'closed'` therefore reports a
// dead stream as healthy, which is exactly the bug this state was added to prevent.

/** A body that stays open until `push`/`close` are called, so a test can hold the
 *  stream mid-flight and decide when it ends. */
function controllableSSE(): {
  response: Response;
  push: (chunk: string) => void;
  close: () => void;
} {
  let controller: ReadableStreamDefaultController<Uint8Array>;
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  const response = new Response(body, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  });
  return {
    response,
    push: (chunk) => controller.enqueue(encoder.encode(chunk)),
    close: () => controller.close(),
  };
}

function setup() {
  const store = createChatStore();
  const stream = controllableSSE();
  const fetchFn = vi.fn(async () => stream.response) as unknown as typeof fetch;
  const api = {
    basePath: '/api/bridge',
    fetchFor: () => fetchFn,
    getValidators: vi.fn(async () => ({})),
    getMessages: vi.fn(async () => ({ model: null })),
  } as unknown as ApiClient;
  const cache = {
    isEnabled: false,
    putSummary: vi.fn(async () => {}),
    deleteSession: vi.fn(async () => {}),
    scheduleTurnsWrite: vi.fn(),
    flushTurnsWrites: vi.fn(async () => {}),
  } as unknown as SessionCache;
  const engine = new SyncEngine({ store, api, cache, sweepIntervalMs: 1_000_000 });
  return { store, engine, stream };
}

const connState = (store: ReturnType<typeof createChatStore>) => store.getState().connState;

/** Poll (real timers, sub-frame budget) until `want` shows up, so the test never
 *  depends on how many microtasks the stream plumbing happens to take. */
async function waitForConn(store: ReturnType<typeof createChatStore>, want: string): Promise<void> {
  for (let i = 0; i < 200; i++) {
    if (connState(store) === want) return;
    await new Promise((r) => setTimeout(r, 5));
  }
  throw new Error(`connState never became ${want} (stuck at ${connState(store)})`);
}

describe('connState — what useConnState() reads', () => {
  it("starts 'idle': the provider boots the prime BEFORE the engine, so this window is real", () => {
    const { store } = setup();
    expect(connState(store)).toBe('idle');
  });

  it("start() -> 'connecting', and a hello frame -> 'open'", async () => {
    const { store, engine, stream } = setup();
    engine.start();
    expect(connState(store)).toBe('connecting');

    stream.push('event: hello\ndata: {}\n\n');
    await waitForConn(store, 'open');

    engine.stop();
  });

  it("a DROPPED stream goes back to 'connecting', NOT 'closed'", async () => {
    const { store, engine, stream } = setup();
    engine.start();
    stream.push('event: hello\ndata: {}\n\n');
    await waitForConn(store, 'open');

    // The server hung up. SyncEngine reconnects with backoff, so the honest report
    // for that window is "connecting" — and a consumer testing `!== 'closed'` would
    // read this as a live stream.
    stream.close();
    await waitForConn(store, 'connecting');
    expect(connState(store)).not.toBe('closed');

    engine.stop();
  });

  it("stop() -> 'closed' (unmount is the only thing that produces it)", async () => {
    const { store, engine, stream } = setup();
    engine.start();
    stream.push('event: hello\ndata: {}\n\n');
    await waitForConn(store, 'open');

    engine.stop();
    expect(connState(store)).toBe('closed');
  });
});
