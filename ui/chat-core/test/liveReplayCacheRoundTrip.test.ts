// Replay of the REAL session whose prompt was drawn once before and once after the
// answer after a tab reload (br_1789665111110648544, turn ...T2BH98PP, measured
// 2026-09-17). Fixtures are log-store's own events for that turn, renumbered into the
// stream's id space, and the 30-turn page the client fetches — nothing synthesized.
//
// The send-then-stream path was already sound: `liveReplayEchoBoundary` covers it, and
// this session passes it at every repair point too. What nothing covered is the CACHE.
// SyncEngine writes the live model to IndexedDB after every streamed batch, so the
// cache holds live-keyed rows (`msg_…_user`); a reload reads the model back into a tail
// whose frame-id map is empty. The merge used that map to tell live rows from page
// history, so the cached prompt passed for history, was kept beside the page's copy
// (`e_…`) and ordered after the page's entries — after the answer. `Entry.origin` is
// the fix: the stamp travels with the entry, through the cache and back.
import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { TurnModel } from '../src/net/types.js';
import type { WireEvent } from '../src/net/wireEvents.js';
import eventsFixture from './fixtures/cache-round-trip-session-events.json';
import pageFixture from './fixtures/cache-round-trip-session-page.json';

const SID = 'br_1789665111110648544';
const LAST_EVENT_BEFORE_THE_TURN = 2695032; // log-store id; turns 1-3 end here

const rows = eventsFixture as Array<Record<string, unknown>>;
const events: WireEvent[] = rows.map((data) => ({
  id: String(data.event_id),
  type: String(data.type),
  data: data as never,
}));
const logStoreIds = rows.map((data) => Number(data.log_store_id));
const page = (pageFixture as unknown as { model: TurnModel }).model;
const PROMPT = String((rows[0]!.result as { text: string }).text);

/** The page as log-store would have served it when its newest event was `cut`. */
function pageAt(cut: number): TurnModel {
  const entries: TurnModel['entries'] = {};
  for (const [id, e] of Object.entries(page.entries)) if (e.eventId <= cut) entries[id] = e;
  const turns = page.turns
    .map((t) => ({ ...t, entryIds: t.entryIds.filter((id) => entries[id]) }))
    .filter((t) => t.entryIds.length > 0);
  return { ...page, turns, entries, validator: { ...page.validator, maxEventId: cut } };
}

/** What IndexedDB hands back: a structured clone of the model, and nothing else. */
const throughTheCache = (model: TurnModel): TurnModel => JSON.parse(JSON.stringify(model)) as TurnModel;

function promptRows(store: ReturnType<typeof createChatStore>) {
  const model = store.getState().turnsBySession.get(SID);
  const visible: { id: string; role?: string; text: string }[] = [];
  if (model) {
    for (const t of model.turns) {
      for (const id of t.entryIds) {
        const e = model.entries[id];
        if (e && !e.duplicate && e.text) visible.push({ id, role: e.role, text: e.text });
      }
    }
  }
  const at = visible.map((v, i) => (v.role === 'user' && v.text === PROMPT ? i : -1)).filter((i) => i >= 0);
  const firstAnswer = visible.findIndex((v, i) => i > (at[0] ?? Infinity) && v.role === 'assistant');
  return {
    count: at.length,
    afterAnswer: firstAnswer >= 0 && at.some((i) => i > firstAnswer),
    ids: at.map((i) => visible[i]!.id),
  };
}

describe('replay: live rows cached mid-turn, read back on reload, then the page lands', () => {
  it('shows the prompt once, before the answer, wherever in the turn the reload falls', () => {
    const failures: string[] = [];
    for (let k = 1; k <= events.length; k++) {
      const beforeReload = createChatStore();
      const live = beforeReload.getState().actions;
      live.setTurns(SID, pageAt(LAST_EVENT_BEFORE_THE_TURN));
      live.appendOptimisticUser(SID, PROMPT, 'creq_test');
      live.applyTailEvents(SID, events.slice(0, k)); // SyncEngine writes the cache after each batch
      const cached = throughTheCache(beforeReload.getState().turnsBySession.get(SID)!);

      const afterReload = createChatStore();
      const a = afterReload.getState().actions;
      a.setTurns(SID, cached); // Prefetcher.hydrateFromCache
      const painted = promptRows(afterReload);
      a.setTurns(SID, pageAt(logStoreIds[k - 1]!)); // the page fetched on open
      const merged = promptRows(afterReload);
      a.applyTailEvents(SID, events); // a fresh tab has no cursor: the server replays the turn
      a.setTurns(SID, page);
      const settled = promptRows(afterReload);

      for (const [step, r] of [['painted', painted], ['merged', merged], ['settled', settled]] as const) {
        if (r.count !== 1 || r.afterAnswer) failures.push(`k=${k} ${step}: ${JSON.stringify(r)}`);
      }
    }
    const shapes = [...new Set(failures.map((f) => f.replace(/^k=\d+ /, '')))];
    expect(failures.length, `${failures.length} failed; shapes: ${shapes.slice(0, 4).join(' | ')}`).toBe(0);
  });

  it('a live row keeps its stamp through the cache, and a page row is stamped as one', () => {
    const store = createChatStore();
    const a = store.getState().actions;
    a.setTurns(SID, pageAt(LAST_EVENT_BEFORE_THE_TURN));
    a.applyTailEvents(SID, events.slice(0, 2));
    const cached = throughTheCache(store.getState().turnsBySession.get(SID)!);
    const origins = new Map<string, number>();
    for (const e of Object.values(cached.entries)) origins.set(String(e.origin), (origins.get(String(e.origin)) ?? 0) + 1);
    expect(origins.get('undefined')).toBeUndefined();
    expect(origins.get('live')).toBeGreaterThan(0);
    expect(origins.get('page')).toBeGreaterThan(0);
  });
});
