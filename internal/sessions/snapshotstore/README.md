# snapshot-store

Point-in-time file snapshot store for the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

Captures file contents before and after tool calls (e.g. Claude Code `Edit`/`Write`) so the UI can render before/after diffs. Metadata lives in SQLite; raw content is stored as content-addressed blobs in a bare git repo, which handles compression and dedup for free.

## Usage

```go
import "github.com/kayushkin/snapshot-store"

s, err := snapshotstore.Open(snapshotstore.Config{
    DBPath: "~/.llm-bridge/snapshots.db",
    GitDir: "~/.llm-bridge/snapshots.git",
})
if err != nil { /* ... */ }
defer s.Close()

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

`PurgeOlderThan(cutoff)` removes SQLite rows. `GC()` runs `git gc` to reclaim blobs that have been unreachable past the 31-day prune-expire window (caller is expected to run purge first, then GC on a slower cadence).

## Layout

```
snapshot-store/
├── store.go       Open/Close/migrate
├── blob.go        git hash-object / cat-file wrapper, IsBinary
├── snapshot.go    Capture/Get/PurgeOlderThan + Snapshot type
└── store_test.go
```
