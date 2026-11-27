# snapshots (formerly snapshot-store)

`internal/sessions/snapshots`, package `snapshots`. It was the standalone snapshot-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `snapshots`. Its record ids did not change.

Point-in-time file snapshot store for the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Captures file contents before and after tool calls (e.g. Claude Code `Edit`/`Write`) so the UI can render before/after diffs. Metadata lives in the Postgres module `snapshots`; raw content is stored as content-addressed blobs in a bare git repo, which handles compression and dedup for free.

## Usage

```go
import "github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots"

// db is the module's pool, provisioned, migrated and opened by the server.
s, err := snapshotstore.New(db, snapshotstore.Config{
    GitDir: "~/.llm-bridge/snapshots.git",
})
if err != nil { /* ... */ }

// On PreToolUse for Edit/Write:
s.Capture(sessionID, toolUseID, snapshotstore.PhaseBefore, filePath)

// On PostToolUse:
s.Capture(sessionID, toolUseID, snapshotstore.PhaseAfter, filePath)

// In the HTTP handler:
before, after, err := s.Get(sessionID, toolUseID)
content, err := s.ReadBlob(before.BlobSHA)
```

## Behavior

- **Missing files** (e.g. `Write` before-phase): recorded with `missing=true`, no blob.
- **Binary files** (null byte in first 8 KiB): recorded with `is_binary=true`, no blob.
- **Oversized files** (> `MaxSize`, default 1 MiB): recorded with `too_large=true`, no blob.

## Retention

`PurgeOlderThan(cutoff)` removes metadata rows. `GC()` runs `git gc` to reclaim blobs that have been unreachable past the 31-day prune-expire window (caller is expected to run purge first, then GC on a slower cadence).

## Layout

```
snapshot-store/
├── store.go       Module, New
├── migrations/    the schema
├── blob.go        git hash-object / cat-file wrapper, IsBinary
├── snapshot.go    Capture/Get/PurgeOlderThan + Snapshot type
└── store_test.go
```

## Moving the SQLite file in

`llm-bridge-server import-snapshots -sqlite <copy of snapshots.db>` copies every row of the SQLite file the server kept until the move into the module. The blobs stay in the git folder, which the store keeps using where it is (`snapshots.git_path`).
