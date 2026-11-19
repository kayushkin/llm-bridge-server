import type { TurnModel } from '../net/types.js';

// The harness's own in-progress todo, read from the transcript.
//
// This file used to be `liveStatus.ts` and derived much more: the tool calls in
// flight and the subagents still running, scanned out of the last turn. Those are
// the session's STATUS and llm-bridge-server publishes them now
// (`SessionStatus.tools` / `.subagents`), along with the one-line input summary
// that `toolCallSummary` produced here — it is `msg.ToolCallSummary` in Go. What
// is left is the one thing that is a line of the conversation rather than a fact
// about the session.

export interface InProgressTodo {
  /** Prefers the item's active form ("Refactoring the parser") over its
   *  imperative form ("Refactor the parser"). Whitespace-collapsed, 120 chars. */
  text: string;
  /** Timestamp of the todo list that named it (RFC3339 + offset). */
  sinceTs: string;
}

const MAX_TODO_CHARS = 120;

function truncate(s: string): string {
  const flat = s.replace(/\s+/g, ' ').trim();
  return flat.length > MAX_TODO_CHARS ? `${flat.slice(0, MAX_TODO_CHARS - 1)}…` : flat;
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' && v.length > 0 ? v : undefined;
}

/** The in-progress item of one TodoWrite input. Read defensively — the input is an
 *  untyped blob from the wire. */
function inProgressTodoText(input: unknown): string | undefined {
  if (typeof input !== 'object' || input === null) return undefined;
  const todos = (input as Record<string, unknown>).todos;
  if (!Array.isArray(todos)) return undefined;
  for (const item of todos) {
    if (typeof item !== 'object' || item === null) continue;
    const record = item as Record<string, unknown>;
    if (record.status !== 'in_progress') continue;
    return asString(record.activeForm) ?? asString(record.content);
  }
  return undefined;
}

// Identity memo, one slot per model: the TurnModel is replaced immutably on every
// mutation, so referential equality is a correct staleness check, and a WeakMap
// keeps the entry alive exactly as long as the model it describes. `null` records
// "scanned, nothing in progress" — a hook reads this on every render.
const todoByModel = new WeakMap<TurnModel, InProgressTodo | null>();

/**
 * The latest todo list wins outright, even when it has no in-progress item: the
 * list is the harness's own current statement of the plan, and reaching further
 * back would resurrect a todo the harness has since replaced. Scanned across
 * turns, newest first — the harness's todo list persists across turns.
 */
export function inProgressTodoFromModel(model: TurnModel | undefined): InProgressTodo | undefined {
  if (!model) return undefined;
  const cached = todoByModel.get(model);
  if (cached !== undefined) return cached ?? undefined;
  let found: InProgressTodo | null = null;
  outer: for (let t = model.turns.length - 1; t >= 0; t--) {
    const turn = model.turns[t];
    if (!turn) continue;
    for (let i = turn.entryIds.length - 1; i >= 0; i--) {
      const entry = model.entries[turn.entryIds[i] ?? ''];
      if (!entry || entry.duplicate) continue;
      if (entry.kind !== 'tool_call' && entry.kind !== 'tool_result') continue;
      if (entry.toolName !== 'TodoWrite' || entry.toolInput === undefined) continue;
      const text = inProgressTodoText(entry.toolInput);
      if (text) found = { text: truncate(text), sinceTs: entry.ts };
      break outer;
    }
  }
  todoByModel.set(model, found);
  return found ?? undefined;
}
