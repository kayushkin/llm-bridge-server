import 'fake-indexeddb/auto';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { IDBFactory } from 'fake-indexeddb';
import { SessionCache } from '../src/cache/SessionCache.js';
import type { TurnModel } from '../src/net/types.js';

// The live stream calls into the cache once per streamed frame, and IndexedDB
// structured-clones its argument ON THE MAIN THREAD. Writing straight through made
// that the single largest source of jank in the client.
//
// Measured 2026-08-26 against the real dashboard, eight session opens in a row:
// `putTurns` ran 2,449 times — about 300 per open, because opening a session replays
// the current turn and every replayed frame rewrote the WHOLE model. Main thread
// blocked 25.6s across those eight switches, against 1.4s in the reducer and 1.1s in
// the OTel annotator. It was not the transcript size and it was not the rendering.
//
// So these cases are about the COUNT of writes, not their content. A test that only
// checked "the newest model reaches disk" passes just as happily on the version that
// wrote three hundred times.

function model(sessionId: string, maxEventId: number): TurnModel {
  return {
    sessionId,
    turns: [],
    entries: {},
    validator: { maxEventId, eventCount: maxEventId, updatedAt: '2026-08-26T00:00:00Z' },
    more: false,
  };
}

describe('cache writes from the live stream are coalesced', () => {
  beforeEach(() => {
    globalThis.indexedDB = new IDBFactory();
    vi.useFakeTimers();
  });

  it('turns a burst of frames into ONE write', async () => {
    const cache = new SessionCache(true);
    const write = vi.spyOn(cache, 'putTurns');

    for (let eventId = 1; eventId <= 300; eventId++) {
      cache.scheduleTurnsWrite(model('br_1', eventId));
    }
    expect(write).not.toHaveBeenCalled(); // nothing goes to disk mid-burst

    await vi.advanceTimersByTimeAsync(SessionCache.TURNS_WRITE_COALESCE_MS);

    expect(write).toHaveBeenCalledTimes(1);
  });

  it('writes the NEWEST model of the burst, not the one that opened the window', () => {
    // Coalescing that kept the first value would cache a tail hundreds of frames stale
    // — worse than writing every time, because it looks like it worked.
    const cache = new SessionCache(true);
    const write = vi.spyOn(cache, 'putTurns').mockResolvedValue();

    cache.scheduleTurnsWrite(model('br_1', 1));
    cache.scheduleTurnsWrite(model('br_1', 2));
    cache.scheduleTurnsWrite(model('br_1', 3));
    vi.advanceTimersByTime(SessionCache.TURNS_WRITE_COALESCE_MS);

    expect(write).toHaveBeenCalledTimes(1);
    expect(write.mock.calls[0][0].validator.maxEventId).toBe(3);
  });

  it('does not let a busy stream defer the write forever', () => {
    // The reason a later call keeps the RUNNING timer instead of restarting it. A
    // trailing debounce that resets on every frame never fires while a turn is
    // streaming, so a tab closed mid-turn would cache nothing at all.
    const cache = new SessionCache(true);
    const write = vi.spyOn(cache, 'putTurns').mockResolvedValue();

    const half = SessionCache.TURNS_WRITE_COALESCE_MS / 2;
    cache.scheduleTurnsWrite(model('br_1', 1));
    vi.advanceTimersByTime(half);
    cache.scheduleTurnsWrite(model('br_1', 2));
    vi.advanceTimersByTime(half);

    expect(write).toHaveBeenCalledTimes(1);
  });

  it('keeps sessions independent', () => {
    const cache = new SessionCache(true);
    const write = vi.spyOn(cache, 'putTurns').mockResolvedValue();

    cache.scheduleTurnsWrite(model('br_1', 1));
    cache.scheduleTurnsWrite(model('br_2', 1));
    vi.advanceTimersByTime(SessionCache.TURNS_WRITE_COALESCE_MS);

    expect(write).toHaveBeenCalledTimes(2);
    expect(write.mock.calls.map((c) => c[0].sessionId).sort()).toEqual(['br_1', 'br_2']);
  });

  it('a flush writes what is queued immediately and cancels its timer', async () => {
    // What a stream ending does. Without it the last state of a finished turn waits out
    // a timer on a stream nobody is left to feed.
    const cache = new SessionCache(true);
    const write = vi.spyOn(cache, 'putTurns').mockResolvedValue();

    cache.scheduleTurnsWrite(model('br_1', 7));
    await cache.flushTurnsWrites();

    expect(write).toHaveBeenCalledTimes(1);
    expect(write.mock.calls[0][0].validator.maxEventId).toBe(7);

    // The queued timer must not fire a second write afterwards.
    vi.advanceTimersByTime(SessionCache.TURNS_WRITE_COALESCE_MS * 2);
    expect(write).toHaveBeenCalledTimes(1);
  });

  it('a flush with nothing queued is a no-op', async () => {
    const cache = new SessionCache(true);
    const write = vi.spyOn(cache, 'putTurns').mockResolvedValue();

    await cache.flushTurnsWrites();

    expect(write).not.toHaveBeenCalled();
  });

  it('writes nothing at all when the cache is disabled', () => {
    const cache = new SessionCache(false);
    const write = vi.spyOn(cache, 'putTurns').mockResolvedValue();

    cache.scheduleTurnsWrite(model('br_1', 1));
    vi.advanceTimersByTime(SessionCache.TURNS_WRITE_COALESCE_MS);

    expect(write).not.toHaveBeenCalled();
  });

  it('really does reach the database, not just the spy', async () => {
    // The cases above count calls, so one of them has to check the write lands —
    // otherwise a coalescer that dropped everything would pass the whole file.
    vi.useRealTimers();
    const cache = new SessionCache(true);

    cache.scheduleTurnsWrite(model('br_real', 42));
    await new Promise((r) => setTimeout(r, SessionCache.TURNS_WRITE_COALESCE_MS + 50));

    const stored = await cache.getTurns('br_real');
    expect(stored?.validator.maxEventId).toBe(42);
  });
});
