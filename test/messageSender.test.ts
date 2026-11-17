import { describe, expect, it } from 'vitest';
import { createChatStore } from '../src/store/ChatStore.js';

describe('who wrote a user message', () => {
  it('carries the sender from a live user_message onto its entry', () => {
    const store = createChatStore();
    store.getState().actions.applyTailEvent('br_1', {
      id: '1',
      type: 'user_message',
      data: {
        event_id: 1,
        message_id: 'm1',
        turn_id: 't1',
        timestamp: '2026-11-16T17:00:00-08:00',
        result: { text: 'hi' },
        sender_principal_id: 'principal_000002',
        sender_display_name: 'Priya Raman',
      },
    });
    const model = store.getState().turnsBySession.get('br_1')!;
    const user = Object.values(model.entries).find((e) => e.role === 'user')!;
    expect(user.senderPrincipalId).toBe('principal_000002');
    expect(user.senderDisplayName).toBe('Priya Raman');
  });
});
