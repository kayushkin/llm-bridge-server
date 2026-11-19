// Opening a conversation in the chat's pane from outside the chat page's own
// components (a pop-up drawn by the layout). The chat reads `?view=` only when
// it mounts and is the only writer of its URL after that, so a link cannot
// switch it; it listens for this event instead.

export const OPEN_CONVERSATION_IN_CHAT_EVENT = 'bridge-ui:open-conversation-in-chat'

export interface OpenConversationInChatDetail {
  roomID: string
}

export function openConversationInChat(roomID: string): void {
  window.dispatchEvent(new CustomEvent<OpenConversationInChatDetail>(OPEN_CONVERSATION_IN_CHAT_EVENT, { detail: { roomID } }))
}
