import { describe, expect, it } from 'vitest'
import { conductorState, conductorStatusText, newestSignalTime, signalsRaisedSince, type ConductorFacts } from '../src/conductorState'

const calm: ConductorFacts = { reachable: true, enabled: true, homeSessionState: 'idle', needsYou: 0, newNeedsYou: 0 }

describe('conductorState', () => {
  it('conducts when nothing is wrong, running or waiting', () => {
    expect(conductorState(calm)).toBe('conducting')
  })
  it('conducts before the first poll has answered', () => {
    expect(conductorState({ ...calm, reachable: null, enabled: null })).toBe('conducting')
  })
  it('conducts while the home session is not known yet', () => {
    expect(conductorState({ ...calm, homeSessionState: null })).toBe('conducting')
  })
  it('says unreachable over everything else', () => {
    expect(conductorState({ ...calm, reachable: false, needsYou: 3, newNeedsYou: 3, homeSessionState: 'running' })).toBe('offline')
  })
  it('raises what newly needs you over a turn in flight', () => {
    expect(conductorState({ ...calm, needsYou: 2, newNeedsYou: 1, homeSessionState: 'tool_running' })).toBe('alert')
  })
  it('keeps conducting past signals already seen', () => {
    expect(conductorState({ ...calm, needsYou: 30, newNeedsYou: 0 })).toBe('conducting')
  })
  it('is busy while the home session runs, starts or runs a tool', () => {
    for (const state of ['running', 'tool_running', 'starting']) {
      expect(conductorState({ ...calm, homeSessionState: state })).toBe('busy')
    }
  })
  it('is not busy while the home session waits on a person', () => {
    expect(conductorState({ ...calm, homeSessionState: 'awaiting_user' })).toBe('conducting')
  })
  it('shows trouble when the home session ended in error or lost its process', () => {
    expect(conductorState({ ...calm, homeSessionState: 'error' })).toBe('trouble')
    expect(conductorState({ ...calm, homeSessionState: 'disconnected' })).toBe('trouble')
    expect(conductorStatusText('trouble', { ...calm, homeSessionState: 'error' })).toBe('its session is in state error')
  })
  it('rests with autonomous mode off', () => {
    expect(conductorState({ ...calm, enabled: false })).toBe('resting')
  })
})

describe('conductorStatusText', () => {
  it('counts what newly needs you in the singular and plural', () => {
    expect(conductorStatusText('alert', { ...calm, needsYou: 5, newNeedsYou: 1 })).toBe('1 new — needs you')
    expect(conductorStatusText('alert', { ...calm, needsYou: 5, newNeedsYou: 4 })).toBe('4 new — need you')
  })
})

describe('signalsRaisedSince and newestSignalTime', () => {
  const raised = ['2026-09-28T10:00:00Z', '2026-09-28T12:00:00Z', '2026-09-27T09:00:00Z']
  it('counts every signal when nothing has been seen', () => {
    expect(signalsRaisedSince(raised, null)).toBe(3)
  })
  it('counts only those raised after what was seen', () => {
    expect(signalsRaisedSince(raised, '2026-09-28T10:00:00Z')).toBe(1)
    expect(signalsRaisedSince(raised, newestSignalTime(raised))).toBe(0)
  })
  it('names the newest raise time, or none', () => {
    expect(newestSignalTime(raised)).toBe('2026-09-28T12:00:00Z')
    expect(newestSignalTime([])).toBe(null)
  })
})
