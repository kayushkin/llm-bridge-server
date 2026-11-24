package memorystore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultRecencyScanTimeout bounds the recent-files scan when the caller's
// context carries no deadline of its own, which is the normal case: a session
// is usually prepared under a request context, and an HTTP request context has
// no deadline unless something sets one.
//
// The bound exists because the scan's cost is a property of the workspace, not
// of the code: on a git repository it answers in milliseconds (measured 3.8ms
// over ~/repos/inber), but on a root with no git history it degrades to a walk
// of the whole tree (measured 7.7 seconds over a home directory, 283,991
// files). Ten seconds is far above the first case and cuts off the second.
const DefaultRecencyScanTimeout = 10 * time.Second

// PrepareSessionConfig configures what gets loaded into memory for a session
type PrepareSessionConfig struct {
	RootDir        string        // Repository root directory
	IdentityFile   string        // Path to agent identity file (optional)
	IdentityText   string        // Direct identity text (used if IdentityFile is empty)
	AgentName      string        // Agent name for identity
	RecencyWindow  time.Duration // How far back to look for recent files (e.g., 24h)
	RecentFilesTTL time.Duration // How long recent file refs live (e.g., 10min)

	// RecencyScanTimeout caps how long the recent-files scan may run. Zero
	// means DefaultRecencyScanTimeout; a negative value means no cap, leaving
	// the scan bounded only by the caller's context.
	RecencyScanTimeout time.Duration
}

// DefaultPrepareSessionConfig returns sensible defaults
func DefaultPrepareSessionConfig(rootDir string) PrepareSessionConfig {
	return PrepareSessionConfig{
		RootDir:            rootDir,
		AgentName:          "agent",
		RecencyWindow:      24 * time.Hour,
		RecentFilesTTL:     10 * time.Minute,
		RecencyScanTimeout: DefaultRecencyScanTimeout,
	}
}

// PrepareSession loads identity and recent files into memory for a new session.
// This replaces the old context.AutoLoad() pattern.
//
// ctx bounds the recent-files scan, which spawns git and can fall back to
// walking the whole workspace tree. The identity and instruction steps read one
// file and write to the local database, so they do not take it.
func (s *Store) PrepareSession(ctx context.Context, cfg PrepareSessionConfig) error {
	// The caller may already have given up before we start; loading identity
	// writes to the store, so ask before writing anything.
	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. Load identity (permanent, always-load)
	if err := s.loadIdentity(cfg); err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	// 2. Load memory usage instructions (permanent, always-load)
	if err := s.loadMemoryInstructions(); err != nil {
		return fmt.Errorf("failed to load memory instructions: %w", err)
	}

	// 3. Load tool registry (permanent, always-load)
	// Note: This will be populated later by engine after tools are built
	// For now, just ensure the structure is ready

	// 4. Load recent files (ephemeral, TTL-based)
	if cfg.RecencyWindow > 0 {
		if err := s.loadRecentFiles(ctx, cfg); err != nil {
			// A scan that failed, or that hit its own timeout, leaves the
			// session without its recent-files hints and is survivable.
			//
			// The caller withdrawing is not the same event and must not be
			// reported as a warning and a successful prepare: nobody is waiting
			// for the result, and the steps above have already written to the
			// store. Ask the caller's context, not the error — the scan's own
			// deadline arrives here looking exactly like a cancellation.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			fmt.Fprintf(os.Stderr, "warning: failed to load recent files: %v\n", err)
		}
	}

	return nil
}

// loadIdentity loads agent identity into memory as an always-load memory
func (s *Store) loadIdentity(cfg PrepareSessionConfig) error {
	var identityText string

	// Try to load from file first
	if cfg.IdentityFile != "" {
		content, err := os.ReadFile(cfg.IdentityFile)
		if err != nil {
			return err
		}
		identityText = string(content)
	} else if cfg.IdentityText != "" {
		identityText = cfg.IdentityText
	} else {
		// Default identity
		identityText = fmt.Sprintf("You are %s, a helpful coding assistant with access to file operations and shell commands.", cfg.AgentName)
	}

	// Save as always-load memory
	return s.Save(Memory{
		ID:         "identity",
		Content:    identityText,
		Tags:       []string{"identity", "always-load"},
		Importance: 1.0,
		Source:     "system",
		AlwaysLoad: true,
	})
}

// loadMemoryInstructions loads memory usage instructions
func (s *Store) loadMemoryInstructions() error {
	instructions := `You have persistent memory across sessions via these tools:
- memory_search: Search your memories before answering questions about past work, preferences, or decisions
- memory_save: Save important information — decisions made, user preferences, project context, lessons learned
- memory_forget: Mark outdated or incorrect memories as forgotten

Guidelines:
- Search memory at the start of conversations about ongoing projects
- Save key decisions and their reasoning
- Save user preferences when explicitly stated
- Don't save trivial or temporary information
- Review and forget outdated memories when you notice them`

	return s.Save(Memory{
		ID:         "memory-instructions",
		Content:    instructions,
		Tags:       []string{"instructions", "memory", "always-load"},
		Importance: 0.9,
		Source:     "system",
		AlwaysLoad: true,
	})
}

// loadRecentFiles loads recently modified file references into memory with TTL
func (s *Store) loadRecentFiles(ctx context.Context, cfg PrepareSessionConfig) error {
	scanCtx, endScan := recencyScanContext(ctx, cfg.RecencyScanTimeout)
	defer endScan()

	// Find recently modified files
	recentFiles, err := FindRecentlyModified(scanCtx, cfg.RootDir, cfg.RecencyWindow)
	if err != nil {
		return err
	}

	// Calculate expiration time
	expiresAt := time.Now().Add(cfg.RecentFilesTTL)

	// Save each file as a lightweight reference
	for _, f := range recentFiles {
		// Skip if file is very recently accessed (likely just saved)
		ageMinutes := int(time.Since(f.ModTime).Minutes())
		
		// Build content stub
		var ageStr string
		if ageMinutes < 60 {
			ageStr = fmt.Sprintf("%d minute%s ago", ageMinutes, pluralString(ageMinutes))
		} else {
			hours := ageMinutes / 60
			ageStr = fmt.Sprintf("%d hour%s ago", hours, pluralString(hours))
		}

		content := fmt.Sprintf("Recently modified (%s): %s", ageStr, f.RelativePath)

		// Determine importance based on recency (more recent = more important)
		importance := 0.5
		if ageMinutes < 60 {
			importance = 0.7 // modified in last hour
		} else if ageMinutes < 360 {
			importance = 0.6 // modified in last 6 hours
		}

		// Extract file extension for tagging
		ext := filepath.Ext(f.RelativePath)
		tags := []string{"recent", "file:" + f.RelativePath}
		if ext != "" {
			tags = append(tags, "ext:"+ext)
		}

		// Save with TTL — deterministic ID for dedup (same file = same key = upsert)
		err := s.Save(Memory{
			ID:         "recent:" + f.RelativePath,
			Content:    content,
			Tags:       tags,
			Importance: importance,
			Source:     "system",
			ExpiresAt:  &expiresAt,
		})
		if err != nil {
			return fmt.Errorf("failed to save recent file %s: %w", f.RelativePath, err)
		}
	}

	return nil
}

// recencyScanContext bounds the recent-files scan.
//
// A caller's deadline always wins when it is the earlier of the two, because
// context.WithTimeout keeps whichever expires first — so a spawn that gives its
// child thirty seconds does not get a scan entitled to ten of them on top.
func recencyScanContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout < 0 {
		return context.WithCancel(ctx)
	}
	if timeout == 0 {
		timeout = DefaultRecencyScanTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// pluralString returns "s" if n != 1
func pluralString(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}