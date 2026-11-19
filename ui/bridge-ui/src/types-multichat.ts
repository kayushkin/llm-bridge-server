// multichat's unified contact list as the Messages pages read it. It is
// hand-written because multichat assembles the contact answer from
// unnamed types in its router, so there is nothing for tygo to render yet.

/** One person on one app: the bridge puppet id `user_id`,
 *  under the name that app shows. `linked_by` and `link_reason` say who tied
 *  it to its person and why; absent when it is linked to no one. */
export interface MultichatContactIdentity {
  user_id: string
  platform: string
  display_name?: string
  avatar_url?: string
  /** One of people-store's link sources (`GET /contacts/links/sources`): operator, phone_number, agent, import. */
  linked_by?: string
  link_reason?: string
}

/** `GET {multichatBasePath}/contacts/unified` — one row per person. A person
 *  is a people-store id (`person_000040`) with people-store's name; an account
 *  linked to no one is a row of its own with no `person_id`.
 *  `person_deleted_at` is set when the operator deleted the person in
 *  people-store. Suggested links are people-store suggestions, decided on the
 *  host's People page, not here. */
export interface MultichatUnifiedContact {
  person_id?: string
  person_deleted_at?: number
  display_name: string
  identities: MultichatContactIdentity[]
  platforms?: string[]
}

import type { ConversationVisibility, MessageReactionGroup } from '@kayushkin/multichat-types'

// The Messages pages (Conversations, Search, Contacts) read the records below.
// They are hand-written for the same reason: the conversation, message and page
// records are structs in multichat's internal/matrix/client.go, which tygo.yaml
// does not render (it renders only internal/db/inbound.go), and the search, tag
// and tag-map answers are unnamed types built inside its router. Each comment
// names the Go source, so the day multichat renders them these copies go.

/** `GET {multichatBasePath}/conversations` — matrix.Conversation. Sorted by
 *  last activity, newest first. */
export interface MultichatConversation {
  room_id: string
  name: string
  topic?: string
  avatar_url?: string
  member_count: number
  /** Every joined member but us and the bridge bots; their user ids are the
   *  keys of the contact tag map. */
  members?: MultichatConversationMember[]
  /** The room has no name of its own, so `name` is its members' display
   *  names joined with ", ". */
  named_after_members?: boolean
  platform?: string
  /** Unix milliseconds; 0 when the room has no message yet. */
  last_activity: number
  last_message?: string
  /** The other person in a one-to-one DM; absent in a group, a server
   *  channel, or a DM whose bridge does not say it is one (Telegram). */
  direct_message_partner_user_id?: string
  /** The Discord channel a Discord room carries, from the bridge database;
   *  absent in any other room. message-store records Discord chats by it. */
  discord_channel_id?: string
  /** The operator's choice for the room: hidden, shown whatever its size, or
   *  left to multichat's auto-hide rule. */
  visibility: ConversationVisibility
  /** What the choice and the rule come to: the list leaves the room out
   *  unless hidden rooms are asked for. */
  hidden: boolean
}

/** One person in a room — matrix.ConversationMember. `display_name` is the
 *  one their member event carries in that room, without the bridge's suffix;
 *  absent when it has none. */
export interface MultichatConversationMember {
  user_id: string
  display_name?: string
}

/** One message in a room — matrix.Message. `msg_type` is Matrix's `msgtype`
 *  (`m.text`, `m.image`, `m.file`…); for a file, `body` is its file name. */
export interface MultichatMessage {
  event_id: string
  /** The Matrix user id of the sender, a bridge puppet or our own account. */
  sender: string
  /** Absent when Synapse had no display name for the sender. */
  sender_name?: string
  body: string
  msg_type: string
  /** Unix milliseconds. */
  timestamp: number
  /** Sent by our own account on that app. Omitted when false. */
  is_me?: boolean
  /** `org.matrix.custom.html` when `formatted_body` is Matrix HTML, else "". */
  format?: string
  formatted_body?: string
  /** A media message's file name, sent only when the sender set one apart
   *  from `body`; then `body` is the caption sent with the file. */
  file_name?: string
  /** The `mxc://` URI of an image, file or video message, else "". */
  media_url?: string
  media_mimetype?: string
  reply_to_event_id?: string
  /** Set on an edit: the message it edits. The edit's body and formatting
   *  are the edited content, so `foldEdits` can put them on the original. */
  replaces_event_id?: string
  /** Reactions logged since multichat's reaction log began, grouped by key. */
  reactions?: MessageReactionGroup[] | null
}

/** `GET {multichatBasePath}/conversations/{room_id}/messages?limit=&from=` —
 *  matrix.MessagePage. `messages` is oldest first; `end` is the token to pass
 *  as `from` for the page before this one. */
export interface MultichatMessagePage {
  messages: MultichatMessage[] | null
  end?: string
  has_more: boolean
}

/** `POST {multichatBasePath}/conversations/{room_id}/send` answers this. */
/** `POST {multichatBasePath}/conversations/{room_id}/send-file` — built in
 *  multichat's internal/api/send_file.go. */
export interface MultichatSendFileAnswer {
  event_id: string
  mxc_uri: string
  filename: string
  mimetype: string
  /** `m.image`, `m.video`, `m.audio` or `m.file`. */
  msgtype: string
  size: number
}

export interface MultichatSendAnswer {
  event_id: string
}

/** One hit of `GET {multichatBasePath}/search?q=` (searchHandler's Result). */
export interface MultichatSearchHit {
  room_id: string
  room_name: string
  platform: string
  sender_name: string
  body: string
  timestamp: number
  event_id: string
}

/** Hits grouped by room (searchHandler's Group). Groups arrive in no order. */
export interface MultichatSearchGroup {
  room_id: string
  room_name: string
  platform: string
  results: MultichatSearchHit[] | null
}

/** `GET {multichatBasePath}/search?q=`. `query` is the term as multichat
 *  matched it: trimmed and lower-cased. The answer also carries
 *  `load_results`, left over from a freight demo, which nothing here reads. */
export interface MultichatSearchAnswer {
  query: string
  total: number
  groups: MultichatSearchGroup[] | null
}

/** `GET {multichatBasePath}/tags` — one tag (tagsHandler's Tag). */
export interface MultichatTag {
  id: number
  name: string
  color: string
}

/** `GET {multichatBasePath}/contacts/tags/bulk` — the tags on each contact
 *  identity, keyed by the Matrix user id a tag was put on. An identity with no
 *  tag is absent. */
export type MultichatContactTagMap = Record<string, MultichatTag[]>

/** `GET {multichatBasePath}/conversations/tags/bulk` — each room's own tags,
 *  keyed by room id. A room with no tag is absent. */
export type MultichatConversationTagMap = Record<string, MultichatTag[]>
