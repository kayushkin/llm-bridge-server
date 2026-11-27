# photo-store

What is known about each owner's own photos and videos: where each original is, when and where it was taken and how far to trust that date, a thumbnail, a description, the faces a model found on it, and who is in it. The originals never move; photo-store keeps the facts, and a thumbnail file in file-store.

- **Import** a folder prepared on the machine that holds the originals, or the photos people sent in chats (through [multichat](https://github.com/kayushkin/multichat)). An import matches photos by the SHA-256 of the original among the owner's own photos, so running it again changes only what moved.
- **Describe** a photo, with who wrote the description. An import never overwrites a description or tags.
- **Keep history.** Every change copies the whole photo to a revision first, and nothing is deleted for good except by a purge: a deleted photo's thumbnail still serves, so a citation keeps drawing.
- **Faces and people.** A face model's findings, links to people-store persons that only move upward unless a person decides, and groups of faces a person names at once.
- **Owners.** Every record has an owner (`owner_principal_id`), and a caller reaches only its own unless it sees every owner.

Each photo has an id like `photo_000001`, so an agent's note or a timeline can cite it, and [bridge-ui](https://github.com/kayushkin/bridge-ui) draws it as a chip with the thumbnail, using the TypeScript types in `ts/` (`./generate-ts.sh` renders them from the Go wire types).

It runs inside llm-bridge-server, at `/photos/`, on Postgres (schema `photos`). `CONTRACT.md` is the route table and the owner rules. The server builds the store with `New(db, thumbnails, people)`, where `thumbnails` keeps the thumbnails' bytes (file-store) and `people` says which persons an owner keeps (people-store), and mounts it with `RegisterHandlers`. `ImportStandaloneDatabase` moves a standalone store's file and thumbnails in.

```bash
POSTGRESTEST_ADMINISTRATOR_URL="$(scripts/test-postgres.sh url)" go test ./internal/connectedrecords/photos/...
```

## Importing

**A folder.** On the machine with the originals, write a directory holding one JPEG thumbnail per photo and an `index.jsonl` with one line per photo: the fields of `ImportPhoto` in `photo.go`, with `thumbnail` naming its JPEG in the directory. Then:

```sh
photo-import -dir <directory> -dry-run   # say what would change
photo-import -dir <directory>
```

**Chats.** `photo-import-messages` reads the images in multichat's message log from rooms of at most `-max-members` people, never public server channels, and can also read a Discord history archive and a Facebook Messenger export. It needs `MULTICHAT_URL` and `MULTICHAT_TOKEN`, and `JOURNAL_STORE_URL` for the Discord-DM and Messenger sources. Run `photo-import-messages -h` for its flags.

## License

MIT. See `LICENSE`.
