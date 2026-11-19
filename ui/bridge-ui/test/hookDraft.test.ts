import { describe, expect, it } from 'vitest'
import type { Hook, HookScope } from '@kayushkin/llm-bridge-types'
import { emptyHookDraft, hookDraftOf, hookWireBodyOf, shadowedHookIDs } from '../src/hookDraft'

const hook = (over: Partial<Hook>): Hook => ({
  id: 'hook_1', harness: 'claude_code', event: 'PreToolUse', matcher: 'Bash', command: 'true',
  scope_kind: 'global', enabled: true, created_at: '', updated_at: '', ...over,
} as Hook)
const NARROWEST_FIRST: HookScope[] = ['session', 'instance', 'global']

describe('hookWireBodyOf', () => {
  it('names the missing field before the server has to', () => {
    const empty = emptyHookDraft(null)
    expect(hookWireBodyOf(empty)).toEqual({ ok: false, error: 'Pick the harness the hook runs on.' })
    expect(hookWireBodyOf({ ...empty, harness: 'claude_code' })).toEqual({ ok: false, error: 'Name the event the hook fires on.' })
    expect(hookWireBodyOf({ ...empty, harness: 'claude_code', event: 'Stop' })).toEqual({ ok: false, error: 'Give the shell command the hook runs.' })
  })
  it('requires a scope id for an instance or a session, and drops one from a global hook', () => {
    const base = { ...emptyHookDraft(null), harness: 'claude_code', event: 'Stop', command: 'date' }
    expect(hookWireBodyOf({ ...base, scopeKind: 'instance' })).toEqual({ ok: false, error: 'Pick the instance this hook applies to.' })
    expect(hookWireBodyOf({ ...base, scopeKind: 'session' })).toEqual({ ok: false, error: 'Give the session id this hook applies to.' })
    const global = hookWireBodyOf({ ...base, scopeKind: 'global', scopeId: 'left over from another pick' })
    expect(global).toEqual({ ok: true, body: { harness: 'claude_code', event: 'Stop', command: 'date', matcher: '', scope_kind: 'global', scope_id: '' } })
  })
  it('round-trips a stored hook through the draft', () => {
    const stored = hook({ scope_kind: 'instance', scope_id: 'inst-cc-local', matcher: 'Edit|Write' })
    const result = hookWireBodyOf(hookDraftOf(stored))
    expect(result).toEqual({ ok: true, body: { harness: 'claude_code', event: 'PreToolUse', command: 'true', matcher: 'Edit|Write', scope_kind: 'instance', scope_id: 'inst-cc-local' } })
  })
})

describe('shadowedHookIDs', () => {
  it('marks the wider hook when a narrower one registers the same harness, event and matcher', () => {
    const hooks = [hook({ id: 'g' }), hook({ id: 'i', scope_kind: 'instance', scope_id: 'inst-1' })]
    expect([...shadowedHookIDs(hooks, NARROWEST_FIRST)]).toEqual(['g'])
  })
  it('leaves hooks alone when the matcher or the event differs', () => {
    const hooks = [hook({ id: 'g' }), hook({ id: 'i', scope_kind: 'instance', scope_id: 'inst-1', matcher: 'Edit' }), hook({ id: 's', scope_kind: 'session', scope_id: 'br_1', event: 'Stop' })]
    expect(shadowedHookIDs(hooks, NARROWEST_FIRST).size).toBe(0)
  })
  it('ignores a disabled hook on either side', () => {
    const hooks = [hook({ id: 'g' }), hook({ id: 'i', scope_kind: 'instance', scope_id: 'inst-1', enabled: false })]
    expect(shadowedHookIDs(hooks, NARROWEST_FIRST).size).toBe(0)
  })
})
