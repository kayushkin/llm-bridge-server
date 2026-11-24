// Package memorystore provides persistent, searchable memory across sessions.
package memorystore

import (
	"context"
	"database/sql"
	"time"
)

// Memory represents a single persistent memory entry.
type Memory struct {
	ID           string    // unique identifier
	Content      string    // the actual text
	Summary      string    // compressed version (nullable, for future compaction)
	OriginalID   string    // pointer to parent memory if compacted (nullable)
	Tags         []string  // tags for categorization
	Importance   float64   // 0-1, how important this memory is
	AccessCount  int       // how many times it's been retrieved
	LastAccessed time.Time // timestamp of last retrieval
	CreatedAt    time.Time // when it was stored
	// Source records who a memory came from. It is written by many callers and
	// read, today, by none: no query filters on it and no code branches on it,
	// so it is a label carried for a reader that does not exist yet. Do not
	// treat a value here as enforced.
	//
	// The values the code actually writes are "system", "user", "agent",
	// "compaction", "tool", "git", "mtime", "auto-reference", "summarization"
	// and "extraction" — this comment used to name a five-value set that was
	// neither what the writers write nor what anything checks, which is worse
	// than no list at all. There is deliberately no constant set here: pinning
	// one would read as validation the store does not perform.
	//
	// An empty Source means nobody said, and it stays empty. It used to be
	// defaulted to "user" on the HTTP path, which turned "unknown provenance"
	// into the most trusted value a memory can carry.
	//
	// Nothing can currently say that a memory came from external tool output
	// (a fetched page, a search result, MCP-returned content) — the class that
	// memory-poisoning attacks arrive through. See noteboard todo for that gap;
	// do not paper over it by reusing "tool", which means an inber tool
	// definition, not tool output.
	Source    string
	Embedding []float64 // vector for semantic search

	// New fields for context unification
	AlwaysLoad bool       // if true, always include in context (e.g., identity)
	ExpiresAt  *time.Time // optional expiration (for ephemeral content like recent files)
	Tokens     int        // pre-computed token count for budget management

	// Reference fields for lazy loading
	RefType   string // "memory" (default), "file", "identity", "repo-map", "tools", "web"
	RefTarget string // file path, URL, or empty for pure memories
	IsLazy    bool   // if true, load content on-demand instead of from DB

	// Orchestrator scoping
	Orchestrator string // "inber", "openclaw", etc. — which orchestrator owns this memory
}

// Store handles persistent memory storage via SQLite.
type Store struct {
	db       *sql.DB
	embedder *Embedder
}

// MemoryStore defines the interface for persistent memory storage.
// Different backends can implement this interface (SQLite, Redis, filesystem, in-memory, etc.).
type MemoryStore interface {
	// Core memory operations
	Save(m Memory) error
	Get(id string) (*Memory, error)
	Search(query string, limit int) ([]Memory, error)
	SearchFiltered(query string, limit int, orchestrator string) ([]Memory, error)
	Forget(id string) error

	// Memory management
	DecayImportance() error
	ListRecent(limit int, minImportance float64) ([]Memory, error)
	Compact(minAge time.Duration, minCount int) ([]CompactionResult, error)

	// Context building and session preparation
	BuildContext(req BuildContextRequest) ([]Memory, int, error)
	PrepareSession(ctx context.Context, cfg PrepareSessionConfig) error

	// Tool registry and usage tracking
	LoadToolRegistry(tools []ToolMetadata) error
	UpdateToolUsageSummary(toolName, summary string, ttlSeconds int64) error

	// Session tracking. SaveSession was retired in Phase II.B —
	// memory-store no longer maintains its own session metadata; the
	// session_id passed to TrackMemoryUsage is an opaque reference to
	// llm-bridge-server's canonical session_id.
	TrackMemoryUsage(memoryID, sessionID string, turnNumber int, usageType string) error

	// Lifecycle
	Close() error
}
