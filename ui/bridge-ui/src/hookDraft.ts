import type { Hook, HookOptions, HookScope } from '@kayushkin/llm-bridge-types'

/** A hook as the editor holds it: every field a string, so an input never
 *  holds `undefined`. `hookWireBodyOf` turns it into what `POST /hooks` and
 *  `PATCH /hooks/{id}` take. */
export interface HookDraft {
  harness: string
  event: string
  matcher: string
  command: string
  scopeKind: HookScope
  scopeId: string
}

export function emptyHookDraft(options: HookOptions | null): HookDraft {
  return {
    harness: options?.harnesses[0]?.harness ?? '',
    event: '',
    matcher: '',
    command: '',
    scopeKind: 'global',
    scopeId: '',
  }
}

export function hookDraftOf(hook: Hook): HookDraft {
  return {
    harness: hook.harness,
    event: hook.event,
    matcher: hook.matcher ?? '',
    command: hook.command,
    scopeKind: hook.scope_kind,
    scopeId: hook.scope_id ?? '',
  }
}

export type HookWireBody = Pick<Hook, 'harness' | 'event' | 'command' | 'scope_kind'> & { matcher: string; scope_id: string }

export type HookDraftResult = { ok: true; body: HookWireBody } | { ok: false; error: string }

/** The checks the server makes, made first so the form says which field is
 *  wrong. The server stays the judge: whatever it refuses is shown verbatim. */
export function hookWireBodyOf(draft: HookDraft): HookDraftResult {
  const harness = draft.harness.trim()
  const event = draft.event.trim()
  const command = draft.command.trim()
  const scopeId = draft.scopeId.trim()
  if (!harness) return { ok: false, error: 'Pick the harness the hook runs on.' }
  if (!event) return { ok: false, error: 'Name the event the hook fires on.' }
  if (!command) return { ok: false, error: 'Give the shell command the hook runs.' }
  if (draft.scopeKind !== 'global' && !scopeId) {
    return { ok: false, error: draft.scopeKind === 'instance' ? 'Pick the instance this hook applies to.' : 'Give the session id this hook applies to.' }
  }
  return {
    ok: true,
    body: {
      harness, event, command,
      matcher: draft.matcher.trim(),
      scope_kind: draft.scopeKind,
      // A global hook carries no scope id.
      scope_id: draft.scopeKind === 'global' ? '' : scopeId,
    },
  }
}

/** Enabled hooks that lose to a narrower one: the same (harness, event,
 *  matcher) registered at more than one scope, where the narrowest wins at
 *  spawn for the sessions it covers. `scopeKindsNarrowestFirst` is the served
 *  order from GET /hook-options. */
export function shadowedHookIDs(hooks: readonly Hook[], scopeKindsNarrowestFirst: readonly HookScope[]): Set<string> {
  const rank = new Map(scopeKindsNarrowestFirst.map((kind, index) => [kind, index] as const))
  const rankOf = (hook: Hook) => rank.get(hook.scope_kind) ?? Number.MAX_SAFE_INTEGER
  const byTriple = new Map<string, Hook[]>()
  for (const hook of hooks) {
    if (!hook.enabled) continue
    const key = JSON.stringify([hook.harness, hook.event, hook.matcher ?? ''])
    byTriple.set(key, [...(byTriple.get(key) ?? []), hook])
  }
  const shadowed = new Set<string>()
  for (const group of byTriple.values()) {
    if (group.length < 2) continue
    const narrowest = Math.min(...group.map(rankOf))
    for (const hook of group) if (rankOf(hook) > narrowest) shadowed.add(hook.id)
  }
  return shadowed
}
