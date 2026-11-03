# memory-store

Persistent memory with full-text search for the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

SQLite-backed memory store that gives AI agents cross-session knowledge retention. Memories are ranked by BM25 relevance over an FTS5 index, weighted by importance and recency, and automatically decayed and compacted over time. A context builder assembles prompt context within a token budget.

```
  ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐
       Agent / Application  (inber, openclaw, llm-bridge)
  └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┬ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘
                                 │
              Save / Search / BuildContext / PrepareSession
                                 │
  ╔══════════════════════════════╪══════════════════════════════╗
  ║                    memory-store                            ║
  ║                              │                             ║
  ║   ┌──────────────────────────▼─────────────────────────┐   ║
  ║   │                    MemoryStore                     │   ║
  ║   │                                                    │   ║
  ║   │  Save ─── upsert with auto-indexing & tagging      │   ║
  ║   │  Search ─ BM25 relevance, weighted by importance   │   ║
  ║   │  BuildContext ─ token-budgeted prompt assembly     │   ║
  ║   │  PrepareSession ─ load identity, tools, files      │   ║
  ║   │  Decay / Compact ─ lifecycle management            │   ║
  ║   │                                                    │   ║
  ║   └──────────────────────────┬─────────────────────────┘   ║
  ║                              │                             ║
  ║   ┌──────────────────────────▼─────────────────────────┐   ║
  ║   │              SQLite + FTS5 (porter)                │   ║
  ║   │        bm25() · importance · recency decay         │   ║
  ║   └────────────────────────────────────────────────────┘   ║
  ╚════════════════════════════════════════════════════════════╝
```

## Quick start

### As a library

```go
import memorystore "github.com/kayushkin/memory-store"

// Open or create a store
store, _ := memorystore.NewStore("/path/to/memory.db")
defer store.Close()

// Save a memory
store.Save(memorystore.Memory{
    ID:         "setup-preferences",
    Content:    "User prefers concise responses without emoji",
    Source:     "user",
    Importance: 0.8,
    Tags:       []string{"preference", "style"},
})

// Full-text search
results, _ := store.Search("response style", 5)

// Build prompt context within a token budget
memories, tokens, _ := store.BuildContext(memorystore.BuildContextRequest{
    Tags:              []string{"preference"},
    TokenBudget:       32000,
    IncludeAlwaysLoad: true,
})
```

### With HTTP handlers

```go
mux := http.NewServeMux()
memorystore.RegisterHandlers(mux, store)
http.ListenAndServe(":8080", mux)
```

When used with [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server), memory-store handlers are mounted automatically.

### As a standalone service

For local development, `cmd/memory-store` boots the same HTTP surface on its own
port:

```sh
go run ./cmd/memory-store           # listens on :8165 by default
MEMORY_STORE_ADDR=:9000 MEMORY_STORE_DB=/tmp/mem.db go run ./cmd/memory-store
```

`MEMORY_STORE_DB` defaults to `$HOME/.config/memory-store/memory.db` (the same
canonical path llm-bridge-server uses). In production the library is mounted
into llm-bridge-server rather than run as this binary; the standalone entrypoint
exists mainly for development and for the clean-checkout smoke below.

### Smoke test

`scripts/e2e-smoke.sh` builds the standalone binary from the current checkout,
boots it against a throwaway DB on a throwaway port, and drives the real HTTP
surface (save → read back → search → forget), asserting on parsed bodies. It is
picked up automatically by the repo-smoke-guard (scheduler job 27), which runs
it from a clean clone nightly and flags any repo whose committed source no
longer boots and answers.

```sh
./scripts/e2e-smoke.sh            # E2E_PORT / E2E_KEEP tunable
```

## API

### HTTP endpoints

Registered via `RegisterHandlers(mux, store)`:

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/memories` | Save or upsert a memory |
| `GET` | `/memories/{id}` | Get a memory by ID |
| `DELETE` | `/memories/{id}` | Forget a memory (soft-delete via importance=0) |
| `POST` | `/memories/search` | Full-text search with optional orchestrator filter |
| `GET` | `/memories/recent` | List recent memories (`?limit=20&min_importance=0.5`) |
| `POST` | `/memories/decay` | Apply importance decay to stale memories |
| `POST` | `/memories/compact` | Compress old, low-access memories |
| `POST` | `/memories/context` | Build token-budgeted prompt context |

### Go interface

All operations are available through the `MemoryStore` interface:

```go
type MemoryStore interface {
    Save(m Memory) error
    Get(id string) (*Memory, error)
    Search(query string, limit int) ([]Memory, error)
    SearchFiltered(query string, limit int, orchestrator string) ([]Memory, error)
    Forget(id string) error
    DecayImportance() error
    ListRecent(limit int, minImportance float64) ([]Memory, error)
    Compact(minAge time.Duration, minCount int) ([]CompactionResult, error)
    BuildContext(req BuildContextRequest) ([]Memory, int, error)
    PrepareSession(cfg PrepareSessionConfig) error
    LoadToolRegistry(tools []ToolMetadata) error
    UpdateToolUsageSummary(toolName, summary string, ttlSeconds int64) error
    SaveSession(sess Session) error
    TrackMemoryUsage(memoryID, sessionID string, turnNumber int, usageType string) error
    Close() error
}
```

## How it works

### Memory structure

Each memory has content, an importance score (0-1), tags, and metadata tracking access count and timestamps. Memories can be scoped to a specific orchestrator for multi-agent isolation.

The `Embedding` field is still written and returned, and nothing ranks on it any more — see the note under **Search** below.

| Field | Description |
|-------|-------------|
| `Content` | Main text content |
| `Importance` | 0-1 relevance score, decays over time |
| `Source` | Origin: `user`, `agent`, `reflection`, `compaction`, `system` |
| `Tags` | Categorization tags |
| `AlwaysLoad` | Permanent context (identity, system instructions) |
| `ExpiresAt` | Optional TTL for ephemeral memories |
| `RefType` | Reference type: `memory`, `file`, `identity`, `repo-map`, `tools`, `web` |
| `RefTarget` | File path or URL for lazy-loaded references |
| `Orchestrator` | Multi-tenant scoping (`inber`, `openclaw`, etc.) |

### Search

Content and summary are indexed into an SQLite FTS5 table with the `porter unicode61` tokenizer, so `scheduling` finds `scheduler`. A query is tokenized, stop words are dropped, and each remaining term is quoted and OR-ed — quoting is what stops a query containing `NEAR`, `OR`, `*` or a stray quote from being read as FTS5 syntax rather than as words.

Search scores combine three signals:

```
score = -bm25(memories_fts) × importance × recencyBoost
```

`bm25()` is negative and orders ascending, so the sign flip turns it into a score that composes with the other two. Recency decays exponentially: `0.99^daysSinceAccess`. Accessing a memory boosts its importance by 1% (capped at 1.0).

Two behaviors are worth knowing:

- **A query that matches nothing returns nothing.** It used to return the store's first `limit` rows sorted by id, because an unmatched query scored 0 against everything and 0 sorts as a tie.
- **When BM25 separates no candidate from another** — every query term appears in every matched memory, so every term has zero IDF — importance and recency decide the order instead of the id tie-break.

#### What this replaced, and by how much

Search used to score the cosine similarity of a 256-bucket hashed bag of words. It was described here and in its own doc comment as TF-IDF, and it had no IDF of any kind: every term counted the same whether it appeared in one memory or in all of them, unrelated words sharing a hash bucket scored as a match, and nothing was stemmed.

`TestBM25OutranksTheHashedBagOfWords` runs both retrievers over one labeled corpus of 40 memories and 10 queries and is the measurement behind the switch:

| metric | BM25 | hashed bag of words |
|---|---|---|
| precision over results returned | **0.833** | 0.533 |
| recall@3 | **1.000** | 0.933 |
| MRR | 1.000 | 1.000 |

MRR is reported and deliberately **not** asserted on. Both retrievers put a relevant memory first on all ten queries, so the old one was never bad at finding the single best match — it was bad at everything after it, padding the rest of the context budget with memories that merely collided in a hash bucket. A metric saturated at 1.000 for both arms cannot fail, and asserting on it would be a green light wired to nothing.

### Context building

`BuildContext` assembles memories into a prompt context that fits within a token budget. Memories are selected in priority order:

1. **AlwaysLoad** — identity, system instructions
2. **Tag-matched** — more matching tags = higher priority
3. **High importance** — above the minimum threshold
4. **Recently accessed** — recency as tiebreaker

Stable memories (identity, system) are placed before volatile ones for prompt cache stability. Large memories are auto-truncated with expansion hints.

### Importance decay

Memories not accessed in 24 hours have their importance multiplied by 0.99 per decay cycle. This gradually deprioritizes stale knowledge while keeping frequently-accessed memories prominent.

### Compaction

Old memories (configurable age) with low access counts and low importance are grouped by tag and merged into single compacted memories. Originals are soft-deleted (importance set to 0).

### Session preparation

`PrepareSession` bootstraps a session by auto-loading:

1. Agent identity (always-load)
2. Memory usage instructions (always-load)
3. Tool registry placeholder (always-load)
4. Recently modified files (ephemeral, TTL-based)

Recent files are detected via `git log` (preferred) or filesystem mtime scan.

### Session and usage tracking

Sessions record agent name, model, token usage, cost, and summary. Memory usage is tracked per-session with turn numbers, enabling analytics on which memories agents actually use.

## Configuration

```go
// Store creation
store, _ := memorystore.NewStore("/path/to/memory.db")

// Or auto-create in .inber/memory.db
store, _ := memorystore.OpenOrCreate("/project/root")
```

### BuildContext defaults

| Parameter | Default |
|-----------|---------|
| `TokenBudget` | 32,000 |
| `MinImportance` | 0.4 (when AlwaysLoad excluded) |
| `TruncatePreview` | 300 chars (~100 tokens) |

### PrepareSession defaults

| Parameter | Default |
|-----------|---------|
| `RecencyWindow` | 24 hours |
| `RecentFilesTTL` | 10 minutes |
| `AgentName` | `"agent"` |

## Dependencies

```
modernc.org/sqlite    — Pure Go SQLite
github.com/google/uuid — UUID generation
```

No external HTTP framework — uses stdlib `net/http` with Go 1.22+ path patterns.

## Part of the llm-bridge ecosystem

This store is one component of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. See the llm-bridge README for the full picture — harness bridges, provider bridges, stores, and example consumers. When used with [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server), memory-store is loaded automatically and its endpoints are mounted on the server.
