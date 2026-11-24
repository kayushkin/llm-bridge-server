package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore/internal/childprocess"
	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore/schema"
)

// RecentFiles returns a tool that lists recently modified files with metadata.
func RecentFiles(rootDir string) Impl {
	return Impl{
		Name:        "recent_files",
		Description: "List files that were recently modified, with metadata like line count, modification time, and importance score. Use this to see what's been actively worked on.",
		InputSchema: schema.Props(
			nil, // no required fields
			map[string]any{
				"since":           schema.Str("Time window to search (e.g., '2h', '1d', '7d'). Default: '24h'"),
				"include_content": schema.Bool("If true, include file contents in addition to metadata. Default: false (metadata only)"),
			},
		),
		Run: func(ctx context.Context, input string) (string, error) {
			var params struct {
				Since          string `json:"since"`
				IncludeContent bool   `json:"include_content"`
			}
			if err := json.Unmarshal([]byte(input), &params); err != nil {
				return "", err
			}

			// Default to 24h
			if params.Since == "" {
				params.Since = "24h"
			}

			// Parse duration
			duration, err := parseDuration(params.Since)
			if err != nil {
				return "", fmt.Errorf("invalid duration '%s': %w", params.Since, err)
			}

			// Find recent files
			files, err := findRecentlyModified(ctx, rootDir, duration)
			if err != nil {
				return "", fmt.Errorf("failed to find recent files: %w", err)
			}

			if len(files) == 0 {
				return fmt.Sprintf("No files modified in the last %s.", params.Since), nil
			}

			// Format output
			return formatRecentFiles(files, params.IncludeContent)
		},
	}
}

type recentFile struct {
	Path         string
	RelativePath string
	ModTime      time.Time
	Source       string // "git" or "mtime"
}

// findRecentlyModified finds files modified within the given duration
func findRecentlyModified(ctx context.Context, rootDir string, since time.Duration) ([]recentFile, error) {
	// Try git first
	gitFiles, err := findRecentlyModifiedGit(ctx, rootDir, since)
	if err == nil && len(gitFiles) > 0 {
		return gitFiles, nil
	}

	// The mtime scan is the fallback for "git could not answer", not for "the
	// caller stopped asking". Without this check a canceled git log fails, and
	// failing is precisely what sends the caller into a full tree walk — so
	// canceling the cheap half would start the expensive one.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	// Fall back to mtime
	return findRecentlyModifiedMtime(rootDir, since)
}

// findRecentlyModifiedGit uses git to find recently modified files
func findRecentlyModifiedGit(ctx context.Context, rootDir string, since time.Duration) ([]recentFile, error) {
	// Check if we're in a git repo
	gitDir := filepath.Join(rootDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return nil, err
	}

	// Git command
	sinceTime := time.Now().Add(-since)
	sinceArg := sinceTime.Format("2006-01-02 15:04:05")

	cmd := childprocess.NewCommand(ctx, "git", "log", "--pretty=format:", "--name-only", "--since", sinceArg)
	cmd.Dir = rootDir

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	// Parse output
	lines := strings.Split(string(output), "\n")
	seenFiles := make(map[string]bool)
	var results []recentFile

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || seenFiles[line] {
			continue
		}

		fullPath := filepath.Join(rootDir, line)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue // File might have been deleted
		}

		if info.IsDir() {
			continue
		}

		seenFiles[line] = true
		results = append(results, recentFile{
			Path:         fullPath,
			RelativePath: line,
			ModTime:      info.ModTime(),
			Source:       "git",
		})
	}

	return results, nil
}

// findRecentlyModifiedMtime uses file modification times to find recent files
func findRecentlyModifiedMtime(rootDir string, since time.Duration) ([]recentFile, error) {
	cutoff := time.Now().Add(-since)
	var results []recentFile

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			baseName := filepath.Base(path)
			if baseName == ".git" || baseName == "node_modules" || baseName == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}

		// Check modification time
		if info.ModTime().After(cutoff) {
			relPath, _ := filepath.Rel(rootDir, path)
			results = append(results, recentFile{
				Path:         path,
				RelativePath: relPath,
				ModTime:      info.ModTime(),
				Source:       "mtime",
			})
		}

		return nil
	})

	return results, err
}

// formatRecentFiles formats the list of recent files
func formatRecentFiles(files []recentFile, includeContent bool) (string, error) {
	var builder strings.Builder

	builder.WriteString(fmt.Sprintf("# Recently Modified Files (%d total)\n\n", len(files)))

	for i, file := range files {
		// Read file to get line count
		content, err := os.ReadFile(file.Path)
		lineCount := 0
		if err == nil {
			lineCount = strings.Count(string(content), "\n") + 1
		}

		// Format time
		timeSince := time.Since(file.ModTime)
		var timeStr string
		if timeSince < time.Minute {
			timeStr = "just now"
		} else if timeSince < time.Hour {
			timeStr = fmt.Sprintf("%dm ago", int(timeSince.Minutes()))
		} else if timeSince < 24*time.Hour {
			timeStr = fmt.Sprintf("%dh ago", int(timeSince.Hours()))
		} else {
			timeStr = fmt.Sprintf("%dd ago", int(timeSince.Hours()/24))
		}

		// Write metadata
		builder.WriteString(fmt.Sprintf("%d. **%s** (%d lines, %s)\n", i+1, file.RelativePath, lineCount, timeStr))

		// Optionally include content
		if includeContent && err == nil {
			builder.WriteString("```\n")
			builder.WriteString(string(content))
			builder.WriteString("\n```\n")
		}

		builder.WriteString("\n")
	}

	return builder.String(), nil
}

// parseDuration parses a duration string like "2h", "1d", "30m"
func parseDuration(s string) (time.Duration, error) {
	// Handle days specially
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}

	// Standard duration parsing
	return time.ParseDuration(s)
}
