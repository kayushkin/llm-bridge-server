# mailstack

Read and write email across Gmail, Outlook (Microsoft Graph) and IMAP accounts, plus local accounts this module owns outright. It runs inside llm-bridge-server, mounted at `/mail/` with its own paths behind the prefix, and keeps its records in the Postgres module `mail` (role `mail_module`, migrations in `mailmodule/migrations`).

Until the move it was a service of its own on `:8195` with two SQLite files, `accounts.db` and `local-mail.db`. `llm-bridge-server import-mail -accounts-sqlite <file> [-local-mail-sqlite <file>]` copies both into the module, keeping every row and number.

## Who may call it

The server it runs in names the caller in the request context (`ContextWithCaller`); no header names it. The internal service — the server's service token, naming nobody — is unrestricted. A principal is checked on every call against principal-store, grant-store and kanban-store in the same process (`PeopleDirectory`):

- an administrator passes everything;
- `can_read_mail` on resource type `mail_account` opens an account's folders, threads, lists and messages; `can_import_mail` also opens imports and mailbox changes, and includes `can_read_mail`;
- with no grant, a person reads only the messages a card they may view links as an `email` entity, `<account>:<id>`;
- everything else is for the internal service and administrators (403 `route_not_open_to_principal`).

An unreadable message is 404, exactly like a missing one. A store that cannot answer is 502, never "holds nothing". `POST /api/ingress/deliveries` takes no caller: a sender presents `X-Mailstack-Ingress-Token`, a credential minted by `POST /api/ingress/credentials` and bound to local accounts.

## Reaching mail providers

Gmail, Outlook and IMAP accounts reach their providers only when the server is built with provider connections on (`mail.provider_connections.enabled` in the bridge, off by default). Off, every read or change on such an account answers 503 `mail_provider_connections_off` and no token is asked of auth-store; local accounts answer either way. Nothing here fetches mail on a timer, and no route sends mail.

## What it keeps

- **The message index** (`messages`): what has been seen of each message, filled as reads happen, for Message-ID lookups and threads.
- **Recorded bodies** (`message_bodies`): the bodies of messages fetched in full, only to back `GET /api/search` and `POST /api/search/similar`. Both rank with BM25, computed in SQL as FTS5's `bm25()` was, with field weights subject 8, sender address 6, sender name 2, body 1; the score is negative and lower is better. Text Postgres cannot hold (bytes that are not UTF-8, a NUL) is kept with U+FFFD in their place.
- **Local accounts**: messages, bodies (text and HTML apart, absent distinct from empty), earlier revisions, import sources, ingress credentials and receipts, purge tombstones and the outbox (`GET /api/events`).
- **Attachments** of local messages: the bytes are in the file-store module, reached in the same process; `local_message_attachments` keeps which file hangs on which revision and never hands a file-store id out.
- **IMAP logins** (`imap_creds`), with their passwords, as the standalone store kept them.
