# message-store

Every chat message one person sent or received, one row each, in time order,
whatever app carried it: phone texts, Skype, Messenger, Discord, and whatever a
live bridge logs. It runs inside llm-bridge-server, at `/messages/`, and
keeps its records in Postgres (schema `messages`). Every record has an owner,
the principal whose messages they are, and a caller reaches only its own.

- **Conversations** (`conversation_000001`): one chat on one platform, with its
  participants by platform handle. A handle points at a person by an id from a
  separate people service; a message's sender is read through that row, so
  naming a handle once names every message it sent.
- **Messages** (`chat_message_000001`): time to the millisecond, sender handle,
  kind, text, attachments (picture ids from a photo service), and the id the
  message had where it came from. `(source, source_ref)` is unique per owner,
  where `source` is the id namespace (`discord`, `skype`…), so the same Discord
  message arriving from a data package and from a live bridge is kept once.
- **Conversation days** (`conversation_day_000001`): one conversation on one
  local day, midnight to midnight where the user lived, with the journal
  transcript that once held it.

Importers live in `cmd/` and post to the store over HTTP through
`internal/importclient`. See `CONTRACT.md` for the routes. The tests need a
Postgres: `POSTGRESTEST_ADMINISTRATOR_URL` (see the repo's
`scripts/test-postgres.sh`).
