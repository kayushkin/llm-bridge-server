# memories (formerly memory-store)

`internal/agents/memories`, package `memories`. It was the standalone memory-store until it moved into llm-bridge-server; on 2026-11-27 its package, route prefix, Postgres module, settings and import subcommand took the name `memories`. Its record ids did not change.

Persistent memory with full-text search for the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem.

A memory store that gives AI agents cross-session knowledge retention. Memories are ranked by BM25 relevance, weighted by importance and recency, and automatically decayed and compacted over time. A context builder assembles prompt context within a token budget.

It is a module of llm-bridge-server: its records live in Postgres (schema `memories`, role `memories_module`, built by `migrations/*.sql`) and its routes are mounted on the server at the same paths as before (`/memories…`), as operator routes. Until 2026-11 it kept a SQLite file and had a standalone command; `llm-bridge-server import-memories -sqlite <file>` copies such a file into the module, keeping every id (`ImportStandaloneDatabase`).

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
  ║   │        Postgres tsvector (Snowball stemmer)        │   ║
  ║   │       BM25 · importance · recency decay            │   ║
  ║   └────────────────────────────────────────────────────┘   ║
  ╚════════════════════════════════════════════════════════════╝
```

## Quick start

### As a library

```go
import "github.com/kayushkin/llm-bridge-server/internal/agents/memories"

// The server provisions, migrates and opens the module, then hands its pool here.
store := memories.New(db)

// Save a memory
store.Save(memories.Memory{
    ID:         "setup-preferences",
    Content:    "User prefers concise responses without emoji",
    Source:     "user",
    Importance: 0.8,
    Tags:       []string{"preference", "style"},
})

// Full-text search
results, _ := store.Search("response style", 5)

// Build prompt context within a token budget
recalled, tokens, _ := store.BuildContext(memories.BuildContextRequest{
    Tags:              []string{"preference"},
    TokenBudget:       32000,
    IncludeAlwaysLoad: true,
})
```

### With HTTP handlers

```go
memories.RegisterHandlers(mux, store)
```

llm-bridge-server mounts them on its own mux.

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

Content and summary are kept as a generated `tsvector` column (`search_document`) with a GIN index, under the text search configuration `memory_text`: every ASCII punctuation mark separates words, as FTS5's `unicode61` tokenizer did, and every word is stemmed with the Snowball English stemmer and none dropped, so `scheduling` finds `scheduler`. A query is tokenized on every rune that is not a letter or a number, stop words are dropped, and each remaining term becomes a quoted tsquery operand, OR-ed — so a query holding `NEAR`, `OR`, `*`, `&` or a stray quote is only ever words. Being a column of the row, the search document can never fall behind a save or need a backfill, as the SQLite store's separate FTS5 index could.

BM25 is computed in the query with FTS5's own formula and constants (`k1` 1.2, `b` 0.75, IDF `ln((N − n + 0.5)/(n + 0.5))` taken as 1e-6 when not positive, document length counted over every word), so a ranking moved from SQLite keeps its order; `TestSearchRanksAsFTS5Bm25Did` holds Search to a real FTS5 index over the ranking corpus. Two differences remain: Snowball and SQLite's Porter stemmer disagree on a few endings, and FTS5's `unicode61` also folded accents, which this does not.

Search scores combine three signals:

```
score = bm25 × importance × recencyBoost
```

Recency decays exponentially: `0.99^daysSinceAccess`. Accessing a memory boosts its importance by 1% (capped at 1.0).

Two behaviors are worth knowing:

- **A query that matches nothing returns nothing.** It used to return the store's first `limit` rows sorted by id, because an unmatched query scored 0 against everything and 0 sorts as a tie.
- **When BM25 separates no candidate from another** — every query term appears in every matched memory, so every term has next to no IDF — importance and recency decide the order instead of the id tie-break.

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
Postgres, through llm-bridge-server's postgresstorage (pgx)
github.com/google/uuid — UUID generation
```

No external HTTP framework — uses stdlib `net/http` with Go 1.22+ path patterns.

## Part of the llm-bridge ecosystem

This store is one component of the [llm-bridge](https://github.com/kayushkin/llm-bridge) ecosystem. See the llm-bridge README for the full picture — harness bridges, provider bridges, stores, and example consumers. It runs inside [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server), which mounts its endpoints.
