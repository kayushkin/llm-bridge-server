import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';
import type { WireEvent } from '../src/net/wireEvents.js';

function userMessage(over: {
  eventId: number;
  messageId: string;
  turnId: string;
  text: string;
  clientRequestId?: string;
}): WireEvent {
  return {
    id: String(over.eventId),
    type: 'user_message',
    data: {
      event_id: over.eventId,
      message_id: over.messageId,
      turn_id: over.turnId,
      timestamp: '2026-07-27T14:00:00-07:00',
      result: { text: over.text },
      ...(over.clientRequestId ? { client_request_id: over.clientRequestId } : {}),
    },
  };
}

function userEntries(store: ReturnType<typeof createChatStore>, sid: string) {
  const model = store.getState().turnsBySession.get(sid)!;
  return Object.values(model.entries).filter((e) => e.role === 'user');
}

describe('stripOptimisticUser — bug-1 hardening', () => {
  it('collapses the optimistic row against a normalized/trimmed server prompt', () => {
    const store = createChatStore();
    const sid = 'br_1';
    store.getState().actions.appendOptimisticUser(sid, 'hello world', 'c1');
    // Server echoes the prompt back with extra whitespace (normalized differently).
    store.getState().actions.applyTailEvent(
      sid,
      userMessage({ eventId: 1, messageId: 'm1', turnId: 't1', text: '  hello   world ' }),
    );
    const users = userEntries(store, sid);
    // Exactly one user row survives — no double-show.
    expect(users).toHaveLength(1);
    const raw = users[0]!.raw as { optimistic?: boolean } | undefined;
    expect(raw?.optimistic).toBeUndefined(); // the canonical row, not the optimistic one
  });

  it('prefers a client-request-id correlation even when the text differs', () => {
    const store = createChatStore();
    const sid = 'br_2';
    store.getState().actions.appendOptimisticUser(sid, 'original text', 'CID-42');
    store.getState().actions.applyTailEvent(
      sid,
      userMessage({
        eventId: 1,
        messageId: 'm1',
        turnId: 't1',
        text: 'server-rewritten prompt entirely different',
        clientRequestId: 'CID-42',
      }),
    );
    expect(userEntries(store, sid)).toHaveLength(1);
  });

  it('does not blindly strip on an empty server text (guard)', () => {
    const store = createChatStore();
    const sid = 'br_3';
    store.getState().actions.appendOptimisticUser(sid, 'keep me', 'c9');
    // Empty result.text with no client id — cannot correlate, so the optimistic row
    // must NOT be dropped by an empty-string match.
    store.getState().actions.applyTailEvent(
      sid,
      userMessage({ eventId: 1, messageId: 'm1', turnId: 't1', text: '' }),
    );
    const optimistic = userEntries(store, sid).filter(
      (e) => (e.raw as { optimistic?: boolean } | undefined)?.optimistic,
    );
    expect(optimistic).toHaveLength(1); // still present
  });
});
