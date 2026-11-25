# activity-store

Keeps what a person did online: searches, pages visited, videos watched, apps
used and places looked up on a map, one activity per thing done at one time.
It imports Google Takeout: My Activity's pages, YouTube's watch and search
history, and Chrome's history and bookmarks.

It runs inside llm-bridge-server, at `/activity-store/`, with its records in
Postgres (schema `activity`, role `activity_module`) and the bytes of its
attachments in the server's file-store. Every record has an owner, the
principal whose activity it is, and a caller reaches only its own records (an
administrator reaches every owner's). Each activity has an id like
`activity_000001`, so a note or a journal timeline can cite it, and bridge-ui
draws that id as a chip using the TypeScript types in `ts/`.

## What it does

- **Import** a Takeout export. Each file is found again among its owner's
  imports by its SHA-256, so importing it twice changes nothing, and an import
  that stopped part way resumes when run again. Voice recordings and pictures
  that come with a card go to file-store.
- **Fold** the same thing seen twice: a YouTube watch in My Activity and in
  YouTube's history, or a page in My Activity and in Chrome's history, is one
  activity with two sightings. Only one owner's records fold together.
- **Search** every word of the titles, links and details (a Postgres
  `tsvector`).
- **Summarize a day** in any time zone, by product (`GET /days/2026-01-05?tz=Europe/Paris`).
- **Keep history.** Deletes are soft and leave a revision; a purge removes
  what an activity said, everywhere, for something that must not be kept.

`CONTRACT.md` lists every route and field.

## Importing

```sh
activity-import-google -dir Takeout -dry-run   # say what it would read
activity-import-google -dir Takeout
```

The command posts to `ACTIVITY_STORE_URL` (default `http://127.0.0.1:8336`,
where the standalone store answered).

## License

MIT. See `LICENSE`.
