import { describe, expect, it } from 'vitest'
import type { SessionAction } from '@kayushkin/chat-core'
import {
  sessionActionCostText,
  sessionActionIsConfirmable,
  sessionActionOutputIsMarkdown,
  sessionActionRunBy,
  sessionActionStatusText,
  sessionActionTarget,
} from '../src/sessionActionView'

const action = (overrides: Partial<SessionAction> = {}): SessionAction => ({
  action_id: 'session_action_000001',
  session_id: 'br_1',
  offer: { label: 'Deploy dash', type: 'deploy', repo_id: 12 },
  command: 'run `bash -l -c ./deploy.sh` in /repos/dash (repo-store repo 12, dash)',
  state: 'offered',
  offered_at: '2026-09-28T12:00:00Z',
  ...overrides,
})

describe('a session action in the chat', () => {
  it('can be pressed only while it waits for its one run', () => {
    expect(sessionActionIsConfirmable(action())).toBe(true)
    for (const state of ['running', 'succeeded', 'failed', 'outcome_unknown'] as const) {
      expect(sessionActionIsConfirmable(action({ state }))).toBe(false)
    }
  })

  it('names who pressed it, or the internal service when nobody was named', () => {
    expect(sessionActionRunBy(action({ run_by_principal_id: 'principal_000001' }))).toBe('principal_000001')
    expect(sessionActionRunBy(action())).toBe('the internal service')
  })

  it('says on the button how the run went', () => {
    expect(sessionActionStatusText(action({ state: 'running' }))).toBe('Running…')
    expect(sessionActionStatusText(action({ state: 'succeeded' }))).toBe('Done')
    expect(sessionActionStatusText(action({ state: 'failed' }))).toBe('Failed')
    expect(sessionActionStatusText(action({ state: 'outcome_unknown' }))).toBe('Outcome unknown')
  })
})

describe('the richer kinds of button', () => {
  it('cannot be pressed when its reviewer rejected its command', () => {
    const command = action({ offer: { label: 'Tidy', type: 'run_command', shell_command: 'rm -rf build' } })
    expect(sessionActionIsConfirmable({ ...command, review: { verdict: 'caution', reasons: 'deletes build', model: 'm', reviewed_at: '' } })).toBe(true)
    expect(sessionActionIsConfirmable({ ...command, review: { verdict: 'reject', reasons: 'deletes too much', model: 'm', reviewed_at: '' } })).toBe(false)
  })

  it('draws a model answer as markdown, and a command output only when the agent said so', () => {
    expect(sessionActionOutputIsMarkdown(action({ offer: { label: 'Ask', type: 'model_call', message: 'q', model: 'm', maximum_cost_usd: 0.2 } }))).toBe(true)
    expect(sessionActionOutputIsMarkdown(action({ offer: { label: 'Go', type: 'background_agent', message: 'q', maximum_cost_usd: 2 } }))).toBe(true)
    expect(sessionActionOutputIsMarkdown(action({ offer: { label: 'List', type: 'run_command', shell_command: 'ls' } }))).toBe(false)
    expect(sessionActionOutputIsMarkdown(action({ offer: { label: 'List', type: 'run_command', shell_command: 'ls', result_format: 'markdown' } }))).toBe(true)
  })

  it('says what it may spend, then what it spent', () => {
    const ask = action({ offer: { label: 'Ask', type: 'model_call', message: 'q', model: 'm', maximum_cost_usd: 0.2 } })
    expect(sessionActionCostText(ask)).toBe('up to $0.20')
    expect(sessionActionCostText({ ...ask, state: 'succeeded', cost_usd: 0.0021 })).toBe('spent $0.0021 of $0.20')
    expect(sessionActionCostText(action())).toBe('')
  })

  it('names the other session a send or a stop acts on', () => {
    const send = action({
      offer: { label: 'Answer it', type: 'send_to_session', target_session_id: 'br_target', message: 'go on' },
    })
    expect(sessionActionTarget(send)).toEqual({ verb: 'Sends to', sessionId: 'br_target' })
    const stop = action({ offer: { label: 'Stop it', type: 'stop_session', target_session_id: 'br_target' } })
    expect(sessionActionTarget(stop)).toEqual({ verb: 'Stops', sessionId: 'br_target' })
    expect(sessionActionTarget(action())).toBeNull()
  })
})
