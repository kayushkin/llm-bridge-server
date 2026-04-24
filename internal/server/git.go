package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

const gitExecTimeout = 10 * time.Second

// GitRepo is one repository entry returned by /sessions/{id}/git/repos.
type GitRepo struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// GitView is the four-pane snapshot returned by /sessions/{id}/git.
type GitView struct {
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	Status       string `json:"status"`
	DiffUnstaged string `json:"diff_unstaged"`
	DiffStaged   string `json:"diff_staged"`
	Log          string `json:"log"`
}

// handleSessionGitRepos returns the repos discovered for the session.
func (s *Server) handleSessionGitRepos(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repos, err := s.discoverGitRepos(sess.BridgeID, sess.Info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]GitRepo, 0, len(repos))
	for _, p := range repos {
		out = append(out, GitRepo{Path: p, Name: filepath.Base(p)})
	}
	writeJSON(w, map[string]any{"repos": out})
}

// handleSessionGit returns status/diff/staged/log for a single repo.
// `?repo=<absolute-path>` selects the repo; defaults to the first discovered.
func (s *Server) handleSessionGit(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repos, err := s.discoverGitRepos(sess.BridgeID, sess.Info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(repos) == 0 {
		http.Error(w, "no git repositories discovered for this session", http.StatusNotFound)
		return
	}

	requested := r.URL.Query().Get("repo")
	repo := repos[0]
	if requested != "" {
		found := false
		for _, p := range repos {
			if p == requested {
				repo = p
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "repo not in discovered set for this session", http.StatusForbidden)
			return
		}
	}

	view, err := collectGitView(r.Context(), repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

// discoverGitRepos returns the deduped, sorted set of git repos this session
// has touched: the session's working directory (if set) plus every directory
// reachable by walking up from `file_path`/`path`/`notebook_path` fields seen
// in tool_call inputs. Returns an empty slice when nothing is found — never
// guesses a default.
func (s *Server) discoverGitRepos(sessionID string, info *msg.SessionInfo) ([]string, error) {
	seen := make(map[string]struct{})
	add := func(p string) {
		if p == "" {
			return
		}
		root := findGitRoot(p)
		if root == "" {
			return
		}
		seen[root] = struct{}{}
	}

	if info != nil && info.WorkingDir != "" {
		add(info.WorkingDir)
	}

	inputs, err := s.store.ListToolCallInputs(sessionID)
	if err != nil {
		return nil, fmt.Errorf("list tool inputs: %w", err)
	}
	for _, raw := range inputs {
		for _, p := range extractToolPaths(raw) {
			add(p)
		}
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// extractToolPaths pulls the conventional path-bearing fields out of a tool
// input blob. Limited to fields the standard Claude tools use; arbitrary
// shell command parsing (Bash) is intentionally skipped — too lossy to be a
// reliable source of truth.
func extractToolPaths(raw json.RawMessage) []string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if v, ok := obj[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// findGitRoot walks up from p (a file or directory) looking for a `.git`
// entry. Returns "" if none is found, the path doesn't exist, or any I/O
// error occurs (callers treat this as "not a repo and don't expose it").
func findGitRoot(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	clean := filepath.Clean(p)

	// If p is a file, start from its parent. If it doesn't exist, walk up
	// the lexical parent chain anyway — files Claude touched might have
	// been deleted, but the enclosing repo can still be valid.
	dir := clean
	if fi, err := os.Stat(clean); err == nil && !fi.IsDir() {
		dir = filepath.Dir(clean)
	} else if err != nil {
		dir = filepath.Dir(clean)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// collectGitView shells out to git four times and returns the combined view.
// Errors from any one call are fatal — callers see a 500 with the raw
// stderr — rather than a partial blob with missing panes.
func collectGitView(parent context.Context, repo string) (*GitView, error) {
	ctx, cancel := context.WithTimeout(parent, gitExecTimeout)
	defer cancel()

	branch, err := runGit(ctx, repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		// Detached HEAD is normal — fall back to the short SHA so the UI
		// has something to show. Distinguished from a real error by the
		// well-known git stderr substring.
		if strings.Contains(err.Error(), "ref HEAD is not a symbolic ref") {
			sha, sherr := runGit(ctx, repo, "rev-parse", "--short", "HEAD")
			if sherr != nil {
				return nil, fmt.Errorf("git rev-parse: %w", sherr)
			}
			branch = "(detached " + strings.TrimSpace(sha) + ")"
		} else {
			return nil, fmt.Errorf("git symbolic-ref: %w", err)
		}
	}
	status, err := runGit(ctx, repo, "status", "--porcelain=v1", "--branch")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	diffUnstaged, err := runGit(ctx, repo, "diff", "--no-color")
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	diffStaged, err := runGit(ctx, repo, "diff", "--cached", "--no-color")
	if err != nil {
		return nil, fmt.Errorf("git diff --cached: %w", err)
	}
	logOut, err := runGit(ctx, repo, "log", "-n", "20",
		"--pretty=format:%h\t%ad\t%an\t%s", "--date=iso-strict")
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	return &GitView{
		Repo:         repo,
		Branch:       strings.TrimSpace(branch),
		Status:       status,
		DiffUnstaged: diffUnstaged,
		DiffStaged:   diffStaged,
		Log:          logOut,
	}, nil
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr != "" {
			return "", fmt.Errorf("%w: %s", err, stderrStr)
		}
		return "", err
	}
	return stdout.String(), nil
}
