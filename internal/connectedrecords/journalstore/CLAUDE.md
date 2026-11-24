# About journal-store

## What it owns

`:8322`, loopback only. The user's own writing: journal entries, blog posts, notes, notes about one person or thing, timelines, and transcripts of conversations the user took part in (`kind`: `journal`, `post`, `note`, `about`, `timeline`, `transcript`, served by `GET /entry-kinds`). Ids are `entry_000001`, registered in reference-store's list of reference types as type `entry`, so dash can turn one into a chip. **This is private writing, kept apart from noteboard on purpose**: noteboard is mostly agent output and every agent reads it. Do not copy entries into noteboard, and do not read them into an agent's output unless the user asked. `CONTRACT.md` is the route table.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Mentions are ref: links

`[Carol](ref:person_000012)` in a body is a mention of `person_000012`. **The id is the truth; the link text is only what was shown when it was written.** Every write reparses the body and rewrites the entry's rows in `mentions`; `GET /mentions?ref=<id>` lists the live entries that mention an id. `entity_type` is the one reference-store type whose `id_patterns` match the ref, or `''` when none or several do. The registry is read at start and every five minutes with `KANBAN_STORE_SERVICE_TOKEN`; a failed read never blocks a write — the mention is stored untyped, `GET /health` says `degraded`, and the next good read backfills. Do not add a list of id prefixes here: the registry is the only list.

## Timelines

A timeline is an entry of kind `timeline`; `about_ref` names the person it follows, or is empty for an event timeline. Its dated items are rows in `timeline_items` (`timeline_item_000001`), with a `precision` from `GET /time-precisions`, a markdown body and `sources` — the ids the item rests on. Body links and sources are mentions, so `GET /mentions?ref=` finds a timeline through its items. `GET /timeline-items?timeline=<id>&timeline=<id>` merges several timelines in time order: that is the overlay dash draws. Items are soft-deleted and revisioned like entries.

## Publishing

`visibility` is `private` (default), `draft` or `published`. Publishing derives a `slug` from the title when none is set (non-ASCII letters kept) and refuses a slug another live published entry holds with a **409** rather than inventing a suffix. `published_at` is the first publication and survives a trip back to draft. `GET /published` and `/published/{slug}` serve only live published entries, for a public site later.

## Nothing is lost

`DELETE` is soft; `POST /entries/{id}/restore` undoes it. Every update, delete, restore, import update and promote-todo first copies the **whole row** to `entry_revisions`, in the same transaction. Unlike noteboard, the snapshot covers every column.

## Imports and promote-todo

`POST /imports` upserts by `(source, source_ref)`: same `source_hash` skips, another hash updates content but **never** visibility, slug or publication date; one bad entry writes nothing; `?dry_run=true` counts and rolls back. Each importer is a command in `cmd/` over a reader in `internal/`, posts through `internal/importclient`, writes private entries, and takes `-dry-run`:

- `journal-import-files -time-zone <zone> <dir>` (reader `internal/fileimport`) imports a directory of `.txt`/`.md`/`.docx`/`.pdf` as `note` entries under source `samsung-notes`. A Samsung Notes export file name (`Groceries _240113_194226.txt`) gives the note's title and time, which beat the first line and any date in the text; `Notes` in that place means untitled.
- `journal-import-facebook-posts -activity <export>/your_facebook_activity -self-name <name> -people … -source-entry <entry id>` imports the user's own Facebook posts as `journal` entries tagged `facebook-post`, source `facebook-posts`. `internal/facebookexport` holds the export's text repair (UTF-8 bytes written as Latin-1 escapes) and the posts reader.
- `journal-timeline-discord-events -discord-signup-store-url … -people-store-url … -operator-discord-id <id> -guild-timeline <guild id>=<entry id> [-matrix-homeserver <host>] [-dry-run]` puts each Discord event the operator **went to** — discord-signup-store's attendance records, never its roster — on the timeline given for its server, listing everyone recorded as going, and on the timeline of every other person who went that people-store links by Discord account and that has one. Items cite the event (`discord_event_…`) and the attendance records (`discord_attendance_…`). A timeline holding any item that cites the event for that date, deleted ones included, gets no other; a hand-written one gets the attendance records added to its sources. The command owns items whose body starts with `Went to [` or `Both went to [` (and the older `Signed up for [` forms): it rewrites them when attendance changes and deletes one for a date the operator has no attendance record for, first giving it a body that starts `No attendance record for this date: `. Only an item deleted with that body is restored when a record comes; an owned item a person deleted stays deleted. A scheduler job runs it; this host's flags are in that job.
- `journal-timeline-calendar-events -calendar-store-url … -people-store-url … -calendar <calendar_…> -timeline <entry id> -operator-person-id <person_…> -time-zone <zone> [-dry-run]` puts each past event of a calendar-store calendar on the given timeline, and on the timeline of every person calendar-store links it to. Items cite the event (`calendar_event_000001`). A series is one item spanning its dates (its rule is read with rrule-go); a changed copy of one date and a cancelled event are left out; an all-day date is read in `-time-zone`. The same never-twice rule as the Discord command. A scheduler job runs it; this host's flags are in that job.

**Chat transcripts moved to message-store on 2026-10-30.** Entries of kind `transcript` (sources `skype`, `discord-package`, `facebook-messenger`, `flip3-sms`, `android-messages`) were one conversation per local day; message-store now keeps every message as its own row, with importers for each of those archives. `journal-point-transcripts-at-messages [-source <source>] [-dry-run]` replaced each transcript's body with a link to its `conversation_day_…` in message-store, keeping the people and photos it linked; the entry keeps its id, its old body stays in its revisions, and it is tagged `moved-to-message-store`. Do not import chat history here again: add it to message-store.

`POST /entries/{id}/promote-todo {"line":n}` makes a checklist line a noteboard todo and rewrites it to `- [ ] [text](ref:<uuid>)`.

# Access and operations

## Who may call it

**No authentication, so it binds 127.0.0.1 only** (`JOURNAL_STORE_ADDR=127.0.0.1:8322` in the unit, the same default in `settings.go`, and `deploy.sh` fails on any other bind). Do not widen it without adding auth first. dash is meant to be the front door. The database directory is 0700 and the file 0600. Unit `journal-store.service`; `KANBAN_STORE_SERVICE_TOKEN` comes from a host-local drop-in in `~/.config/systemd/user/journal-store.service.d/`, never from the tracked unit. The noteboard note tagged `journal-store` says which drop-in this host uses.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/journal-store`, root package `journalstore`, server in `cmd/journal-store`. SQLite at `~/.config/journal-store/journal-store.db`. Every variable it reads is declared once in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`) and served read-only at `GET /settings`; `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails on an undeclared `os.Getenv`, a set undeclared `JOURNAL_STORE_` variable stops the start, and `deploy.sh` checks the running service's environment before it stops it. Declare nothing `Editable`. ⚠️ **`go test` and `go build` need `-tags sqlite_fts5`**; the `Makefile` and `deploy.sh` set it. `deploy.sh` also fails when `/health` is not `ok`, which is how a wrong token shows. Tests use synthetic files only: **no real note belongs in this repo.**

## Generated TypeScript types

`./generate-ts.sh` runs tygo over `entry.go` and `vocabulary.go` (`tygo.yaml` excludes the rest) and writes `ts/model.ts`, published source-only as `@kayushkin/journal-store-types` (`file:../journal-store/ts`). A wire type lives in `entry.go` or it is not rendered.

## Public repository: nothing personal

This repo is meant to be public. Nothing about the user's own writing goes in it — not in code, tests, comments, commit messages or this prompt: no text from an entry, names of people or chats, account names or ids, places the user lived or went, paths to the user's archives, home directories, email addresses or tokens. Tests use made-up people and data (`Carol`, `alice_s`, `Me Self`, `Europe/Lisbon`). Importer examples use placeholders (`<entry id>`, `<name>`). A fact an agent here needs about this host's archives or setup goes in the noteboard note tagged `journal-store`.
