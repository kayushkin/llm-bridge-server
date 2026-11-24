package agentstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The scan walks all of $HOME, so it finds every copy of a prompt file as well
// as the file itself: git worktrees, the Go module cache, vendored upstream
// repos. Those are not prompts anyone here maintains. An ignore rule claims
// them. A claimed row is stamped with the rule's id and drops out of
// ListTrackedFiles; it is never deleted, and clearing the rule brings it back
// on the next scan.

const (
	TrackedFileIgnoreKindPathPattern = "path_pattern"
	// TrackedFileIgnoreKindGitWorktree claims a file inside a linked git
	// worktree. It is detected, not pattern-matched: in a linked worktree
	// `.git` is a file that points at the real repository, in a clone it is a
	// directory. Worktree folders here are named every which way
	// (`-wt-`, `.wt-`, `_wt-`, `scheduler-cv-…`), so no name pattern holds.
	TrackedFileIgnoreKindGitWorktree = "git_worktree"
)

type TrackedFileIgnoreRule struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	PathPattern string `json:"path_pattern,omitempty"` // path_pattern rules: relative to $HOME, '*' matches any run of characters
	Reason      string `json:"reason"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
}

func (s *Store) ListTrackedFileIgnoreRules() ([]TrackedFileIgnoreRule, error) {
	rows, err := s.db.Query(`SELECT id, kind, path_pattern, reason, enabled, created_at FROM tracked_file_ignore_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrackedFileIgnoreRule{}
	for rows.Next() {
		var rule TrackedFileIgnoreRule
		var enabled int
		if err := rows.Scan(&rule.ID, &rule.Kind, &rule.PathPattern, &rule.Reason, &enabled, &rule.CreatedAt); err != nil {
			return nil, err
		}
		rule.Enabled = enabled == 1
		out = append(out, rule)
	}
	return out, rows.Err()
}

// CreateTrackedFileIgnoreRule adds a rule, or returns the existing one with
// the same kind and pattern. It stamps nothing until the next scan.
func (s *Store) CreateTrackedFileIgnoreRule(kind, pathPattern, reason string) (*TrackedFileIgnoreRule, error) {
	pathPattern = strings.TrimSpace(pathPattern)
	reason = strings.TrimSpace(reason)
	switch kind {
	case TrackedFileIgnoreKindPathPattern:
		if pathPattern == "" || strings.HasPrefix(pathPattern, "/") {
			return nil, fmt.Errorf("path_pattern is required and is relative to $HOME, got %q", pathPattern)
		}
	case TrackedFileIgnoreKindGitWorktree:
		if pathPattern != "" {
			return nil, fmt.Errorf("a %s rule takes no path_pattern", kind)
		}
	default:
		return nil, fmt.Errorf("kind must be %q or %q", TrackedFileIgnoreKindPathPattern, TrackedFileIgnoreKindGitWorktree)
	}
	if reason == "" {
		return nil, fmt.Errorf("reason is required: say why these files are not prompts")
	}
	if _, err := s.db.Exec(`
		INSERT INTO tracked_file_ignore_rules (kind, path_pattern, reason, enabled, created_at) VALUES (?, ?, ?, 1, ?)
		ON CONFLICT (kind, path_pattern) DO NOTHING`, kind, pathPattern, reason, now()); err != nil {
		return nil, err
	}
	var rule TrackedFileIgnoreRule
	var enabled int
	err := s.db.QueryRow(`SELECT id, kind, path_pattern, reason, enabled, created_at FROM tracked_file_ignore_rules WHERE kind=? AND path_pattern=?`, kind, pathPattern).
		Scan(&rule.ID, &rule.Kind, &rule.PathPattern, &rule.Reason, &enabled, &rule.CreatedAt)
	rule.Enabled = enabled == 1
	return &rule, err
}

func (s *Store) SetTrackedFileIgnoreRuleEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE tracked_file_ignore_rules SET enabled=? WHERE id=?`, boolToInt(enabled), id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func ignorePathPatternMatches(pattern, relativePath string) bool {
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	matched, err := regexp.MatchString("^"+strings.Join(parts, ".*")+"$", relativePath)
	return err == nil && matched
}

// insideLinkedGitWorktree walks up from path to the nearest `.git` and reports
// whether it is a file. It stops at home.
func insideLinkedGitWorktree(path, home string) bool {
	for directory := filepath.Dir(path); strings.HasPrefix(directory, home) && directory != home; directory = filepath.Dir(directory) {
		info, err := os.Lstat(filepath.Join(directory, ".git"))
		if err == nil {
			return !info.IsDir()
		}
	}
	return false
}

// trackedFileIgnoreRuleFor returns the id of the first enabled rule that
// claims path, 0 when none does.
func trackedFileIgnoreRuleFor(path, home string, rules []TrackedFileIgnoreRule) int64 {
	relativePath := strings.TrimPrefix(path, home+string(os.PathSeparator))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		switch rule.Kind {
		case TrackedFileIgnoreKindPathPattern:
			if ignorePathPatternMatches(rule.PathPattern, relativePath) {
				return rule.ID
			}
		case TrackedFileIgnoreKindGitWorktree:
			if insideLinkedGitWorktree(path, home) {
				return rule.ID
			}
		}
	}
	return 0
}

// stampTrackedFileIgnore writes which rule, if any, claims the row, and
// returns that rule's id.
func (s *Store) stampTrackedFileIgnore(fileID int64, path, home string, rules []TrackedFileIgnoreRule) (int64, error) {
	ruleID := trackedFileIgnoreRuleFor(path, home, rules)
	var value any
	if ruleID != 0 {
		value = ruleID
	}
	_, err := s.db.Exec(`UPDATE tracked_files SET ignored_by_rule_id=? WHERE id=? AND COALESCE(ignored_by_rule_id,0) != ?`, value, fileID, ruleID)
	return ruleID, err
}
