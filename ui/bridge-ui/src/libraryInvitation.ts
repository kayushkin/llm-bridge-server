// Inviting the other person in a one-to-one conversation to the host's
// library: the host's invitation route makes them a login and a one-time
// sign-in link, and the conversation posts the link to them.

/** What the host's invitation route answers (kayushkin.com's
 *  `POST /api/viewer/invitations`). */
export interface LibraryInvitation {
  url: string
  expires_at: string
  display_name: string
  principal_id: string
  principal_created: boolean
  added_to_group: boolean
}

/** The message that carries the link. It says the link works once and for how
 *  long, so the person knows not to forward it and to use it soon. */
export function libraryInvitationMessage(url: string): string {
  return `Here is your sign-in link to my library: ${url}\nIt works once, within 7 days, and opens the library in your browser.`
}
