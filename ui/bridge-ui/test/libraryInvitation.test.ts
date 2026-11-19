import { describe, expect, it } from 'vitest'
import { libraryInvitationMessage } from '../src/libraryInvitation'

describe('libraryInvitationMessage', () => {
  it('carries the link whole, secret and all', () => {
    const url = 'https://kayushkin.com/signin#a-one-time-secret'
    expect(libraryInvitationMessage(url)).toContain(url + '\n')
  })
})
