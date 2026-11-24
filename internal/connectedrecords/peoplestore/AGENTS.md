# About people-store

## What it owns

`127.0.0.1:8321`. **The operator decided on 2026-10-01 that this store owns "person"**: the people in the operator's own life — friends, family, coworkers, everyone they chat with — for a journal and for messages, with their chat handles, emails and relationships. Each person gets an id, `person_000001`, registered in kanban-store's entity-type registry as type `person`, so a chat message that names one becomes a reference chip. **principal-store is only who may log in, hold a grant or be assigned a card**; a person may point at a principal with `principal_id`, and this store never copies a principal's name or email. multichat links its bridge accounts to persons here, not to principals. Routes are rooted at `/`. `CONTRACT.md` is the route table.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Persons, identities and relationships

A person carries `display_name`, `birthday` (`YYYY-MM-DD`, or `--MM-DD` when the year is unknown), `how_met`, `tags` and an optional `principal_id`, which must answer 200 at principal-store or the write is refused (400 when principal-store says no such principal, 502 when it cannot answer). **An identity is one handle** — `email`, `phone`, `matrix_user`, `discord` or `url`, served by `GET /identity-kinds` — stored normalized, and **one handle belongs to at most one person**: adding it to a second person is a 409 naming the owner. `GET /identities/resolve?kind=&value=` finds the owner; `GET /identities?kind=` pages through every handle of a kind. **Every link records `linked_by`** (`GET /link-sources`: `operator`, `phone_number`, `agent`, `import`), a `reason` and `linked_at`. Only the operator overrides a **refusal** — "this handle is not this person", written when the operator removes a handle, moves it away, or rejects a suggestion — and only the operator may `move` a handle between persons; every other source gets a 409 and must leave it to a person. A relationship is stored once per fact, "from is to's kind", and each side lists it with the kind the other person is to them (`GET /relationship-kinds` carries the inverses).

## No hard delete, and revisions

`DELETE /persons/{id}` stamps `deleted_at`; `POST /persons/{id}/restore` clears it. **A deleted person still answers `GET /persons/{id}`**, so whatever points at them keeps resolving; lists hide them unless `include_deleted=true`. Every update, delete, restore and identity change first writes a revision holding the person and their identities as they were. Identity and relationship rows are deleted outright; nothing joins on their ids.

## Suggestions and the multichat importer

**Two ways in: a direct link, or an accepted suggestion.** A direct link (`POST /persons/{id}/identities`, or `identities` on `POST /persons`) comes from the operator or from an automatic source that is sure — multichat's phone-number matcher, the review agent at 9 to 1 — and says who and why. **Everything less sure is a suggestion, and this is the one suggestion queue**; multichat has none of its own since 2026-10-01. An importer (`multichat`, `google_contacts`, `phone_messages`) never proposes a handle twice; a reviewer (`agent`) must give a reason and may suggest a handle an importer suggestion already holds. When a handle in a waiting suggestion is attached some other way it moves to `payload.covered`, and a suggestion left empty becomes `superseded`. `POST /suggestions/{id}/accept` with no `person_id` makes a new person; with one it extends that person; it is all or nothing. `~/bin/people-import-multichat` reads multichat's `GET /api/contacts/unified` and proposes one suggestion per group, each account a `matrix_user` handle labeled with its platform. multichat groups accounts by this store's `person_id`, so a linked group is already attached and skipped: never accept the rest in bulk without a person looking. After proposing it reports each contact handle's activity to `PUT /handle-activity` — messages sent and newest `sent_at` from multichat's whole message log, `direct_chat` and room `last_activity` from its one-to-one conversations — and `GET /suggestions?sort=activity` sums that over a suggestion's handles at read time, so a reviewer sees the people the operator talks to first. Which scheduler jobs run the importer and the review agent on this host is in the noteboard note tagged `people-store`.

## The Google Contacts importer

`~/bin/people-import-contacts -export-dir DIR` reads every `.vcf` (vCard 3.0 or 4.0) under a Google Takeout Contacts directory, merges the cards that are one person (same name words and a shared phone or email; Takeout repeats a contact in each label's file), and proposes one `google_contacts` suggestion per contact with a phone or email nobody has. It sends every handle, owned ones too, so the store's own rule names their owners in `matched_person_ids`. **A name is never a link**: when no handle is known but the name matches a person (the same words in any order, or the same first and last word; a one-word name matches only that one word), the reason names the person so the operator can merge into them. Starred contacts say so in the reason; a birthday is only proposed there, never written. Phones become E.164: a number with `+` as written, one without only under `-north-american-numbers` and only when it is a valid NANP number; service codes are skipped, and the raw spelling stays in `source_detail.phones`. Google's "Other contacts" (no categories, no phone: addresses Gmail kept) are skipped unless `-include-other-contacts`. Photos are left alone: this store has no place for one. `-dry-run` reads the store and prints the outcome counts and examples without writing. It needs only `PEOPLE_STORE_URL`.

## The contact review queue

The contact review agent (a scheduler job, started by a trigger job from multichat's repo; ids in the noteboard note tagged `people-store`) works from `GET /reviews/pending`: handles whose activity shows the operator talks with them, not yet reviewed and not claimed in the last 30 minutes. It claims a batch (`POST /reviews/claim`), links what it would bet 9 to 1 on through multichat's `POST /api/contacts/links` (which writes here with `linked_by: agent`), writes `agent` suggestions here for what it half-believes, and marks the batch reviewed (`POST /reviews`). The claim, the queue and the suggestions live here together so two sessions never take the same handles. The reviews multichat had recorded were copied here on 2026-10-01.

# Access and operations

## Who may call it

**No authentication, so it binds 127.0.0.1 only** (`PEOPLE_STORE_ADDR=127.0.0.1:8321` in the unit), and `deploy.sh` fails if it finds the port anywhere else. Do not widen the bind without adding auth first. Unit `people-store.service`, binary `~/bin/people-store`, database `~/.config/people-store/people-store.db`. The importer needs `PEOPLE_STORE_URL`, `MULTICHAT_URL`, `AUTH_STORE_URL` and `AUTH_STORE_TOKEN`, and resolves multichat's token from auth-store provider `multichat`.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/people-store`, root package `peoplestore`, server in `cmd/people-store`, importers in `cmd/people-import-multichat` and `cmd/people-import-contacts`. ⚠️ **`go test` and `go build` need `-tags sqlite_fts5`**; the `Makefile` and `deploy.sh` set it. The service's three variables are declared in `settings.go` with llm-bridge `servicesettings` (hence `replace ../llm-bridge` in `go.mod`) and served read-only at `GET /settings`; the multichat importer's four and the contacts importer's one are declared there too. `TestEveryEnvironmentVariableTheServiceReadsIsDeclared` fails on an undeclared read, a set `PEOPLE_STORE_` variable nobody declared stops the service at boot, and `deploy.sh` checks the running service's environment for one before it stops it. Search is FTS5 over name, how_met, tags and identity values, written from Go in the same transaction as each change (`reindexPerson`). Deploy only with `./deploy.sh` from `main`.

## Generated TypeScript types

`./generate-ts.sh` runs tygo over the root package and writes `ts/model.ts`, published source-only as `@kayushkin/people-store-types` (`file:../people-store/ts`). Only `model.go` and `vocabulary.go` are rendered; `tygo.yaml` excludes the rest. A wire type belongs in `model.go`.

## Public repository: nothing personal

This repo is meant to be public. Nothing about the user's own data goes in it — not in code, tests, comments, commit messages or this prompt: no names of real people, their chat handles, emails, phone numbers, relationships or tags, no names of chats, accounts, servers or machines, no home directories, counts taken from the live database, or tokens. Tests and examples use made-up people and handles (`Alice`, `Bob`, `alice@example.com`, `@telegram_1:chat.example.com`, `+1 202 555 0100`). A fact an agent here needs about this host — scheduler job ids, what the migrations moved — goes in the noteboard note tagged `people-store`.
