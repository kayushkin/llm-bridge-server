# file-store

Uploaded bytes: a ticket's attachment, a mail's attachment, a chat's image.
One small service owns them so that no other store grows a blob column, and so
that size limits, content types, deduplication and deletion are decided once.

`127.0.0.1:8317`. [CONTRACT.md](CONTRACT.md) is the route table.

## What it is, and what it is not

A **file** is one upload: these bytes, under this name, put here by this
service for this thing of its own (`owner_service`, `owner_ref`). Ids are
`file_000001`. The bytes are on disk under `blobs/`, named by their SHA-256, so
identical uploads share one blob; the row is what has an owner and a lifetime.

**An id is given out once.** Ids come from a counter, not from the highest row:
a purge removes the row, and the next upload would otherwise be handed the
purged file's id — so a reference to the old file, left in any other store,
would open somebody else's upload. This was found on the first live run, where
a ticket's attachment came back as the `file_000001` the deploy's own smoke
probe had just used and purged.

**It does not decide who may read a file.** It knows nothing about tickets,
mail or chat. A file is readable exactly when the service that owns the thing
it hangs on says so, and that service is the caller: kanban-store checks the
caller's access to the card and then fetches the bytes here with the service
token. So:

- it binds to loopback, and `deploy.sh` fails if it is listening anywhere else;
- every route but `/health` needs `X-File-Store-Service-Token`;
- **it must never be proxied to a browser**, and its token must never reach an
  agent's environment. Whoever holds the token reads every file.

## An upload is somebody else's bytes

A person's upload shown from the dashboard's origin would run with the
dashboard's cookies. Every download is therefore an attachment of type
`application/octet-stream`, with sniffing off and a sandbox policy, unless the
caller asks `?inline=true` for a file whose declared type is on a short list of
types a browser only displays. A type a browser runs cannot be added to that
list. The filename is never used as a path here, and is still refused unless it
is a plain name, because the next program to save the file will use it as one.

## Deleting

`DELETE` is reversible: the row and the bytes stay, and `POST …/restore` brings
the file back as it was. `?hard=true` destroys the row, and the blob with it
when no other row — live or deleted — still names those bytes. It is the only
thing here that cannot be undone.

## Running it

Configuration is declared in `settings.go` and read through
`llm-bridge/servicesettings`; `GET /settings` describes it. The process refuses
to start on a `FILE_STORE_` variable it does not declare.

| Variable | |
|---|---|
| `FILE_STORE_ADDR` | default `127.0.0.1:8317` |
| `FILE_STORE_DATA_DIR` | default `~/.config/file-store`: `file-store.db` and `blobs/` |
| `FILE_STORE_SERVICE_TOKEN` | required, at least 32 characters |
| `FILE_STORE_MAXIMUM_FILE_BYTES` | seeds `maximum_file_bytes` once (25 MiB); after that the stored value decides — `PUT /settings/maximum_file_bytes` |
| `FILE_STORE_INLINE_CONTENT_TYPES` | seeds `inline_content_types` once, the same way |

The token is host-local and never in the tracked unit:

```
~/.config/file-store-tokens.env                                   (mode 600)
    FILE_STORE_SERVICE_TOKEN=…
~/.config/systemd/user/file-store.service.d/service-token.conf
    [Service]
    EnvironmentFile=/home/<user>/.config/file-store-tokens.env
```

A calling service gets the same file through a drop-in of its own. It is **not**
the shared `principal-gating-tokens.env`: llm-bridge-server reads that one, and
every agent session inherits its variables.

## Development

```
go test ./...
./generate-ts.sh      # file.go → ts/model.ts, @kayushkin/file-store-types
./deploy.sh           # from main only; tests, installs, proves a file round-trips
```

## License

MIT — see [LICENSE](LICENSE).
