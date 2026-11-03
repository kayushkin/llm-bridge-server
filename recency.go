package memorystore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// waitDelayAfterGitExits bounds how long Wait blocks on git's output pipe after
// git has exited or the context is done. It only elapses if something git
// started is still holding that pipe open; without it a canceled command can
// leave the caller blocked on a pipe no live process is writing to.
const waitDelayAfterGitExits = 5 * time.Second

// RecentFile represents a recently modified file
type RecentFile struct {
	RelativePath string
	AbsolutePath string
	ModTime      time.Time
	Source       string // "git" or "mtime"
}

// FindRecentlyModified finds files modified within the given duration.
// Tries git first, falls back to filesystem mtime.
//
// Both strategies are open-ended — one spawns git, the other walks the entire
// tree — so both stop when ctx is done. A root with no git history is the
// expensive case: measured on this box, walking a home directory visited
// 283,991 files in 7.7 seconds, and before ctx reached here nothing could
// interrupt that.
func FindRecentlyModified(ctx context.Context, rootDir string, since time.Duration) ([]RecentFile, error) {
	// Try git first
	gitFiles, err := findRecentlyModifiedGit(ctx, rootDir, since)
	if err == nil && len(gitFiles) > 0 {
		return gitFiles, nil
	}

	// The mtime scan is the expensive strategy and it is reached by the cheap
	// one failing — and a canceled git log fails, so a cancellation asks for
	// the expensive strategy by exactly the same signal a real git failure
	// does. Refusing here says which of the two we are answering.
	//
	// This is a statement of intent, not a fix: deleting it changes nothing
	// observable, because the walk it guards checks the same context on its
	// first entry and stops there. It earns its place only if a later strategy
	// is added that does not check for itself. Measured — with this line gone
	// the canceled scan still returns in microseconds; with the walk's own
	// check gone as well it walks all 12,000 files.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	// Fall back to mtime
	return findRecentlyModifiedMtime(ctx, rootDir, since)
}

// findRecentlyModifiedGit uses git to find recently modified files
func findRecentlyModifiedGit(ctx context.Context, rootDir string, since time.Duration) ([]RecentFile, error) {
	// Check if we're in a git repo
	gitDir := filepath.Join(rootDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		return nil, err
	}

	// Git command to find files modified in the last N seconds
	sinceTime := time.Now().Add(-since)
	sinceArg := sinceTime.Format("2006-01-02 15:04:05")

	// The command is a fixed argv, not a shell, so canceling it has no
	// grandchildren to leave behind and the direct kill exec.CommandContext
	// performs is enough. A tool that runs caller-supplied shell commands needs
	// more than this — see tool-store's internal/childprocess, which owns the
	// process group and escalates SIGINT to SIGKILL.
	cmd := exec.CommandContext(ctx, "git", "log", "--pretty=format:", "--name-only", "--since", sinceArg)
	cmd.Dir = rootDir
	cmd.WaitDelay = waitDelayAfterGitExits

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	
	// Parse output and deduplicate
	fileMap := make(map[string]bool)
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			fileMap[line] = true
		}
	}
	
	// Get actual file mod times
	var results []RecentFile
	for relPath := range fileMap {
		fullPath := filepath.Join(rootDir, relPath)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue // File might have been deleted
		}
		
		// Only include if still within the time window
		if time.Since(info.ModTime()) <= since {
			results = append(results, RecentFile{
				RelativePath: relPath,
				AbsolutePath: fullPath,
				ModTime:      info.ModTime(),
				Source:       "git",
			})
		}
	}
	
	return results, nil
}

// findRecentlyModifiedMtime scans filesystem for recently modified files
func findRecentlyModifiedMtime(ctx context.Context, rootDir string, since time.Duration) ([]RecentFile, error) {
	cutoff := time.Now().Add(-since)
	var results []RecentFile

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		// Checked per entry rather than per directory: a single directory can
		// hold enough entries that a per-directory check is not a bound.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil // Skip errors
		}

		// Skip directories
		if info.IsDir() {
			// Skip common ignore patterns
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == "vendor" || base == ".openclaw" || base == ".inber" || base == "logs" || base == "test-results" {
				return filepath.SkipDir
			}
			return nil
		}
		
		// Check if modified recently
		if info.ModTime().After(cutoff) {
			relPath, err := filepath.Rel(rootDir, path)
			if err != nil {
				relPath = path
			}
			
			results = append(results, RecentFile{
				RelativePath: relPath,
				AbsolutePath: path,
				ModTime:      info.ModTime(),
				Source:       "mtime",
			})
		}
		
		return nil
	})
	if err != nil {
		// A truncated scan is not a short list of recent files. Returning what
		// was collected so far would report a partial answer as a complete one.
		return nil, err
	}

	return results, nil
}

// FormatRecentFiles formats recent files into a human-readable string
func FormatRecentFiles(files []RecentFile) string {
	if len(files) == 0 {
		return "No recently modified files."
	}
	
	var builder strings.Builder
	builder.WriteString("# Recently Modified Files\n\n")
	
	for _, file := range files {
		timeSince := time.Since(file.ModTime)
		var timeStr string
		
		if timeSince < time.Minute {
			timeStr = "just now"
		} else if timeSince < time.Hour {
			mins := int(timeSince.Minutes())
			timeStr = fmt.Sprintf("%d minute%s ago", mins, plural(mins))
		} else if timeSince < 24*time.Hour {
			hours := int(timeSince.Hours())
			timeStr = fmt.Sprintf("%d hour%s ago", hours, plural(hours))
		} else {
			days := int(timeSince.Hours() / 24)
			timeStr = fmt.Sprintf("%d day%s ago", days, plural(days))
		}
		
		builder.WriteString(fmt.Sprintf("- %s (%s, via %s)\n", file.RelativePath, timeStr, file.Source))
	}
	
	return builder.String()
}

// plural returns "s" if count != 1, empty string otherwise
func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}