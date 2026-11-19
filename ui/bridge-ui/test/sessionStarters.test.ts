import { describe, expect, it } from 'vitest'
import { initialsOf, sessionStarterOf, starterChipLabel } from '../src/sessionStarters'
import { userActorLabel } from '../src/components/chat/TurnList'

describe('who started a session', () => {
  it('draws initials of the name, and a gear for no person', () => {
    expect(initialsOf('Priya Raman')).toBe('PR')
    expect(initialsOf('dot.ICO')).toBe('D')
    expect(sessionStarterOf({ principalId: 'principal_000002', displayName: 'Priya Raman' })).toEqual({
      principalId: 'principal_000002', initials: 'PR', title: 'Started by Priya Raman',
    })
    expect(sessionStarterOf({ principalId: '', displayName: '' }).initials).toBe('⚙')
  })
  it('labels a chip by name, by id when there is no name, and "no person" for none', () => {
    expect(starterChipLabel({ principalId: 'principal_000002', displayName: 'Priya Raman' })).toBe('Priya Raman')
    expect(starterChipLabel({ principalId: 'principal_000009', displayName: '' })).toBe('principal_000009')
    expect(starterChipLabel({ principalId: '', displayName: '' })).toBe('no person')
  })
})

describe('who wrote a message', () => {
  it('names the recorded sender, says You for the row still being sent, and User when none was recorded', () => {
    expect(userActorLabel({ senderDisplayName: 'Priya Raman', senderPrincipalId: 'principal_000002', raw: undefined })).toBe('Priya Raman')
    expect(userActorLabel({ senderPrincipalId: 'principal_000002', raw: undefined })).toBe('principal_000002')
    expect(userActorLabel({ raw: { optimistic: true } })).toBe('You')
    expect(userActorLabel({ raw: { type: 'user_message' } })).toBe('User')
  })
})
