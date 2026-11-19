import type { SessionSummary } from '../net/types.js';
import type {
  ManagedSessionWire,
  SessionListFrame,
  WireEvent,
  WireEventData,
} from '../net/wireEvents.js';

// SSE transport (decision D6). Fetch + ReadableStream (not native EventSource) so
// we get auth'd headers and Last-Event-ID resume. Two entrypoints, mirroring the
// two-stream model: one global list stream + one per-session stream for the
// ACTIVE session only. Ported from bridge-ui `bridgeSSE.ts` — same framing, same
// backend streams (`GET /session-events`, `GET /sessions/{id}/events`).

interface SSEFrame {
  type: string;
  data: string;
  id?: string;
}

async function* readFrames(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<SSEFrame> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let current: SSEFrame = { type: '', data: '' };
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() ?? '';
      for (const line of lines) {
        if (line === '') {
          if (current.data || current.type) yield current;
          current = { type: '', data: '' };
        } else if (line.startsWith('event:')) {
          current.type = line.slice(6).trim();
        } else if (line.startsWith('data:')) {
          current.data += (current.data ? '\n' : '') + line.slice(5).trim();
        } else if (line.startsWith('id:')) {
          current.id = line.slice(3).trim();
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}

/** Connect a per-session event stream. Resumes from `lastEventId` via the
 *  `Last-Event-ID` header so a reconnect misses nothing. */
export async function* connectSessionSSE(
  fetchFn: typeof fetch,
  basePath: string,
  sessionId: string,
  lastEventId?: string,
  signal?: AbortSignal,
): AsyncGenerator<WireEvent> {
  const headers: Record<string, string> = { Accept: 'text/event-stream' };
  if (lastEventId) headers['Last-Event-ID'] = lastEventId;

  const res = await fetchFn(`${basePath}/sessions/${sessionId}/events`, { headers, signal });
  if (!res.ok) throw new Error(`SSE connect failed: ${res.status} ${res.statusText}`);
  if (!res.body) throw new Error('No response body');

  for await (const frame of readFrames(res.body)) {
    if (!frame.data) continue;
    let data: WireEventData;
    try {
      data = JSON.parse(frame.data) as WireEventData;
    } catch {
      continue; // skip unparseable frames
    }
    if (frame.id) {
      yield { id: frame.id, type: frame.type || data.type || 'message', data };
    } else {
      yield { type: frame.type || data.type || 'message', data };
    }
  }
}

function summaryFromManaged(m: ManagedSessionWire): SessionSummary {
  return {
    sessionId: m.session_id,
    state: m.state ?? '',
    harness: m.harness ?? '',
    instanceId: m.instance_id ?? '',
    type: m.type ?? '',
    purpose: m.purpose ?? '',
    mode: m.mode ?? '',
    folderName: m.folder_name ?? '',
    displayName: m.display_name ?? '',
    agentId: m.agent_id ?? '',
    principalId: m.principal_id ?? '',
    updatedAt: m.updated_at ?? '',
    createdAt: m.created_at ?? '',
    harnessSessionId: m.harness_session_id ?? '',
    managerSessionId: m.manager_session_id ?? '',
    ...(m.spend_usd !== undefined ? { spendUsd: m.spend_usd } : {}),
    ...(m.working_dir !== undefined ? { workingDir: m.working_dir } : {}),
    ...(m.status ? { status: m.status } : {}),
  };
}

/** Connect the global session-list stream. Yields normalized frames
 *  (`hello` / `upsert` / `delete`); upserts carry a projected SessionSummary. */
export async function* connectListSSE(
  fetchFn: typeof fetch,
  basePath: string,
  signal?: AbortSignal,
  lastEventId?: string,
): AsyncGenerator<SessionListFrame & { summary?: SessionSummary; id?: string }> {
  const headers: Record<string, string> = { Accept: 'text/event-stream' };
  if (lastEventId) headers['Last-Event-ID'] = lastEventId;
  const res = await fetchFn(`${basePath}/session-events`, { headers, signal });
  if (!res.ok) throw new Error(`list SSE connect failed: ${res.status} ${res.statusText}`);
  if (!res.body) throw new Error('No response body');

  for await (const frame of readFrames(res.body)) {
    let data: Record<string, unknown> = {};
    if (frame.data) {
      try {
        data = JSON.parse(frame.data) as Record<string, unknown>;
      } catch {
        continue;
      }
    }
    if (frame.type === 'hello') {
      // `resume` is the server's answer to the Last-Event-ID we sent: `replayed`
      // (every missed frame follows), `gap` (more were missed than it keeps — the
      // caller must re-read the sessions), or `none` (we sent no id). An older
      // server sends no field, which reads as `none`.
      const resume = data.resume === 'replayed' || data.resume === 'gap' ? data.resume : 'none';
      yield { type: 'hello', resume };
    } else if (frame.type === 'upsert' && data.session) {
      const wire = data.session as ManagedSessionWire;
      yield { type: 'upsert', session: wire, summary: summaryFromManaged(wire), ...(frame.id ? { id: frame.id } : {}) };
    } else if (frame.type === 'delete' && data.session_id) {
      yield { type: 'delete', sessionId: String(data.session_id), ...(frame.id ? { id: frame.id } : {}) };
    } else if (frame.type === 'signal' && data.session_id) {
      yield { type: 'signal', sessionId: String(data.session_id), ...(frame.id ? { id: frame.id } : {}) };
    } else if (frame.type === 'session_brief' && data.session_id) {
      yield { type: 'session_brief', sessionId: String(data.session_id), ...(frame.id ? { id: frame.id } : {}) };
    }
  }
}

export { summaryFromManaged };
