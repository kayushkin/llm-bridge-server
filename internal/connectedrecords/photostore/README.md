# photo-store

A small Go service that keeps what is known about your own photos and videos:
where each original is, when and where it was taken and how far to trust that
date, a thumbnail, and a description. The originals never move; photo-store
keeps only a JPEG thumbnail and the facts, in SQLite.

It is one of the stores of the [llm-bridge](https://github.com/kayushkin/llm-bridge)
ecosystem. Each photo has an id like `photo_000001`, so an agent's note or a
timeline can cite it, and [bridge-ui](https://github.com/kayushkin/bridge-ui)
draws that id as a chip with the thumbnail, using the TypeScript types in
`ts/`. It works on its own too: it is a plain HTTP API.

## What it does

- **Import** a folder prepared on the machine that holds the originals, or the
  photos people sent you in chats (through
  [multichat](https://github.com/kayushkin/multichat)). An import matches photos
  by the SHA-256 of the original, so running it again changes only what moved.
- **Describe** a photo, with who wrote the description. An import never
  overwrites a description or tags.
- **Keep history.** Every change copies the whole row to a revision first, and
  nothing is deleted for good: a deleted photo's thumbnail still serves, so a
  citation keeps drawing.
- **Search** by date range, folder, words in the description or tags, and
  whether a photo has GPS.

`CONTRACT.md` lists every route and field.

## Build

photo-store reads its settings through llm-bridge's `servicesettings` package,
and `go.mod` points at a checkout beside this one:

```sh
git clone https://github.com/kayushkin/llm-bridge
git clone https://github.com/kayushkin/photo-store
cd photo-store
go test ./...
go build ./cmd/photo-store ./cmd/photo-import ./cmd/photo-import-messages
```

## Run

```sh
./photo-store            # listens on 127.0.0.1:8329
curl -s localhost:8329/health
```

| Variable | Default | |
|---|---|---|
| `PHOTO_STORE_ADDR` | `127.0.0.1:8329` | listen address |
| `PHOTO_STORE_DATA_DIR` | `~/.config/photo-store` | the database and thumbnails |

`GET /settings` describes every setting and the value in force.

**There is no authentication.** Keep it on loopback, as the default does, and
put a front door that checks who is asking in front of it if anyone else
should reach it.

To run it as a systemd user service, copy the binary to `~/bin` and
`photo-store.service` to `~/.config/systemd/user/`, then
`systemctl --user enable --now photo-store`.

## Importing

**A folder.** On the machine with the originals, write a directory holding one
JPEG thumbnail per photo and an `index.jsonl` with one line per photo: the
fields of `ImportPhoto` in `photo.go`, with `thumbnail` naming its JPEG in the
directory. Then:

```sh
photo-import -dir <directory> -dry-run   # say what would change
photo-import -dir <directory>
```

**Chats.** `photo-import-messages` reads the images in
[multichat](https://github.com/kayushkin/multichat)'s message log from rooms of
at most `-max-members` people, never public server channels, and can also read
a Discord history archive and a Facebook Messenger export. It needs
`MULTICHAT_URL` and `MULTICHAT_TOKEN`, and `JOURNAL_STORE_URL` for the
Discord-DM and Messenger sources. Run `photo-import-messages -h` for its flags.

## License

MIT. See `LICENSE`.
