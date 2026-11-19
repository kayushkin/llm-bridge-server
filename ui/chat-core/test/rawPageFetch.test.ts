import { describe, expect, it } from 'vitest';
import { ApiClient } from '../src/net/ApiClient.js';
import { createChatStore } from '../src/store/ChatStore.js';
import type { SessionSummary, TurnModel } from '../src/net/types.js';

// Two message pages now exist and they are not interchangeable.
//
// `/messages` is PROJECTED by log-store: duplicate entries dropped, `raw` stripped,
// and a `sourceGroups` index carrying the one fact the dropped entries were needed
// for. Measured 2026-08-25 on one real session, that is 0.95 MB against 9.91 MB — the
// Turns view was being sent ten times what it draws.
//
// `/messages/raw` is the unprojected model. The Raw pane renders `entry.raw` directly
// and the Timeline is the audit view over every stored event, so both need it and both
// ask for it by name.
//
// The trap this file exists for: which page a session was loaded from is NOT visible in
// the model. A projected model and a raw model are the same shape — the projected one
// simply holds fewer entries — so a Raw pane handed a projected model renders a
// plausible, quietly incomplete transcript with nothing anywhere reporting a problem.
// `rawTurnsLoaded` is that missing fact, and these cases pin it.

function recordingClient(): { api: ApiClient; urls: string[] } {
  const urls: string[] = [];
  const api = new ApiClient({
    basePath: '/api/bridge',
    fetch: (async (url: string) => {
      urls.push(String(url));
      return { ok: true, status: 200, json: async () => ({ model: {} }) };
    }) as unknown as typeof fetch,
  });
  return { api, urls };
}

function summary(id: string): SessionSummary {
  return {
    sessionId: id,
    state: 'idle',
    harness: 'claudecode',
    instanceId: 'inst1',
    type: 'interactive',
    purpose: 'chat',
    mode: 'events',
    folderName: '',
    displayName: id,
    agentId: '',
    managerSessionId: '',
    updatedAt: '2026-08-25T00:00:00Z',
    createdAt: '2026-08-25T00:00:00Z',
  };
}

function model(id: string, chars = 1000): TurnModel {
  return {
    sessionId: id,
    turns: [{ id: `${id}-t1`, role: 'user', ts: '2026-08-25T00:00:00Z', entryIds: [`${id}-e1`] }],
    entries: {
      [`${id}-e1`]: {
        id: `${id}-e1`,
        turnId: `${id}-t1`,
        role: 'user',
        kind: 'text',
        source: 'harness',
        eventId: 7,
        ts: '2026-08-25T00:00:00Z',
        text: 'x'.repeat(chars),
        duplicate: false,
        primary: true,
      },
    },
    validator: { maxEventId: 7, eventCount: 1, updatedAt: '2026-08-25T00:00:00Z' },
    more: false,
  };
}

describe('the two message pages are addressed separately', () => {
  it('the default page asks for /messages', async () => {
    const { api, urls } = recordingClient();
    await api.getMessages('br_1');
    expect(new URL(urls[0], 'http://x').pathname).toBe('/api/bridge/sessions/br_1/messages');
  });

  it('the unprojected page asks for /messages/raw', async () => {
    const { api, urls } = recordingClient();
    await api.getMessagesRaw('br_1');
    expect(new URL(urls[0], 'http://x').pathname).toBe('/api/bridge/sessions/br_1/messages/raw');
  });

  it('the raw page carries a bound too', async () => {
    // Same reason the default page does — see messagesAlwaysBounded.test.ts. An
    // unbounded request reaches the legacy shape, measured at 306 MB for one session,
    // and this is the page that is already ten times heavier.
    const { api, urls } = recordingClient();
    await api.getMessagesRaw('br_1');
    expect(new URL(urls[0], 'http://x').searchParams.get('limit')).toBe(
      String(ApiClient.DEFAULT_MESSAGE_TURNS),
    );
  });

  it('paging older on the raw page keeps both the cursor and the bound', async () => {
    const { api, urls } = recordingClient();
    await api.getMessagesRaw('br_1', { before: 4200 });
    const params = new URL(urls[0], 'http://x').searchParams;
    expect(params.get('before')).toBe('4200');
    expect(params.get('limit')).toBe(String(ApiClient.DEFAULT_MESSAGE_TURNS));
  });
});

describe('the store records which page a session was loaded from', () => {
  it('marks a session only when the RAW page landed', () => {
    const store = createChatStore();
    const { actions } = store.getState();

    actions.setTurns('br_projected', model('br_projected'));
    actions.setTurns('br_raw', model('br_raw'), { raw: true });

    expect(store.getState().rawTurnsLoaded.has('br_projected')).toBe(false);
    expect(store.getState().rawTurnsLoaded.has('br_raw')).toBe(true);
  });

  it('a later projected fetch does not un-mark a raw-loaded session', () => {
    // The raw page is a SUPERSET. Once a session has been read in full, a Turns-view
    // fetch merging over it leaves it complete — clearing the flag would send the Raw
    // pane back to the network for a page it already has.
    const store = createChatStore();
    const { actions } = store.getState();

    actions.setTurns('br_1', model('br_1'), { raw: true });
    actions.setTurns('br_1', model('br_1'));

    expect(store.getState().rawTurnsLoaded.has('br_1')).toBe(true);
  });

  it('forgets the mark when the transcript is evicted', () => {
    // The mark is a claim about what is IN MEMORY, not about what was once fetched.
    // Left behind, the fetch guard reads "already have the raw page" for a session
    // whose model has been evicted — no fetch fires and the pane renders empty.
    const store = createChatStore({ turnRetentionBytes: 1, turnRetentionMinSessions: 1 });
    const { actions } = store.getState();

    actions.upsertSession(summary('br_old'));
    actions.setActive('br_old');
    actions.setTurns('br_old', model('br_old', 5000), { raw: true });
    expect(store.getState().rawTurnsLoaded.has('br_old')).toBe(true);

    for (let n = 0; n < 5; n++) {
      const id = `br_new_${n}`;
      actions.upsertSession(summary(id));
      actions.setActive(id);
      actions.setTurns(id, model(id, 5000));
    }

    expect(store.getState().turnsBySession.has('br_old')).toBe(false);
    expect(store.getState().rawTurnsLoaded.has('br_old')).toBe(false);
  });

  it('forgets the mark when the session is deleted', () => {
    const store = createChatStore();
    const { actions } = store.getState();

    actions.upsertSession(summary('br_1'));
    actions.setTurns('br_1', model('br_1'), { raw: true });
    actions.removeSession('br_1');

    expect(store.getState().rawTurnsLoaded.has('br_1')).toBe(false);
  });
});
