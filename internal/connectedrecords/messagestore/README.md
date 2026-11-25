# message-store

Every chat message one person sent or received, one row each, in time order,
whatever app carried it: phone texts, Skype, Messenger, Discord, and whatever a
live bridge logs. A small Go service over SQLite, loopback only, with no auth of
its own: put a front door that adds auth in front of it.

- **Conversations** (`conversation_000001`): one chat on one platform, with its
  participants by platform handle. A handle points at a person by an id from a
  separate people service; a message's sender is read through that row, so
  naming a handle once names every message it sent.
- **Messages** (`chat_message_000001`): time to the millisecond, sender handle,
  kind, text, attachments (picture ids from a photo service), and the id the
  message had where it came from. `(source, source_ref)` is unique, where
  `source` is the id namespace (`discord`, `skype`…), so the same Discord message
  arriving from a data package and from a live bridge is kept once.
- **Conversation days** (`conversation_day_000001`): one conversation on one
  local day, midnight to midnight where the user lived, with the journal
  transcript that once held it.

Importers live in `cmd/` and post through `internal/importclient`. See
`CONTRACT.md` for the routes.

Build and test need the `sqlite_fts5` tag: `go test -tags sqlite_fts5 ./...`.

`go.mod` points at [llm-bridge](https://github.com/kayushkin/llm-bridge) in a
folder beside this one, so clone both side by side before you build:

```bash
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/message-store
cd message-store && go test -tags sqlite_fts5 ./...
```
