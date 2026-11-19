import { describe, expect, it } from 'vitest';
import { summaryFromManaged } from '../src/sync/sse.js';
import type { ManagedSessionWire } from '../src/net/wireEvents.js';

// The list stream's upsert is one of the four ways a summary row arrives. The
// summary page carries `spendUsd`; an upsert that dropped `spend_usd` would
// replace a row's spend with nothing every time the session moved.
describe('summaryFromManaged — spend', () => {
  it('keeps the session spend an upsert carries', () => {
    const summary = summaryFromManaged({ session_id: 'br_1234567890123456', spend_usd: 1.25 } as ManagedSessionWire);
    expect(summary.spendUsd).toBe(1.25);
  });

  it('leaves spend absent when the upsert has none, rather than writing zero', () => {
    const summary = summaryFromManaged({ session_id: 'br_1234567890123456' } as ManagedSessionWire);
    expect('spendUsd' in summary).toBe(false);
  });
});
