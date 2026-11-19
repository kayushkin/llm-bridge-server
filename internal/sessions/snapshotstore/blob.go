package snapshotstore

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// initBlobRepo ensures the bare git repo exists and is configured so that
// `git gc` never prunes blobs newer than the snapshot retention window.
func (s *Store) initBlobRepo() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found on PATH: %w", err)
	}

	headPath := filepath.Join(s.gitDir, "HEAD")
	if _, err := os.Stat(headPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(s.gitDir, 0o755); err != nil {
		return err
	}
	if out, err := s.git("init", "--bare", s.gitDir).CombinedOutput(); err != nil {
		return fmt.Errorf("git init --bare: %w: %s", err, out)
	}

	// Disable background gc (caller drives it); keep prune grace window wide
	// so caller has time to delete SQLite rows before blobs become eligible.
	cfgs := [][2]string{
		{"gc.auto", "0"},
		{"gc.pruneExpire", "31.days.ago"},
		{"gc.reflogExpire", "never"},
		{"gc.reflogExpireUnreachable", "never"},
	}
	for _, kv := range cfgs {
		if out, err := s.git("config", kv[0], kv[1]).CombinedOutput(); err != nil {
			return fmt.Errorf("git config %s: %w: %s", kv[0], err, out)
		}
	}
	return nil
}

// WriteBlob stores content as a git blob and returns its SHA-1.
func (s *Store) WriteBlob(content []byte) (string, error) {
	cmd := s.git("hash-object", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadBlob returns the raw bytes of a blob by SHA.
func (s *Store) ReadBlob(sha string) ([]byte, error) {
	if !isHex(sha) {
		return nil, fmt.Errorf("invalid sha %q", sha)
	}
	out, err := s.git("cat-file", "-p", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file %s: %w", sha, err)
	}
	return out, nil
}

// HasBlob reports whether a blob exists in the store.
func (s *Store) HasBlob(sha string) (bool, error) {
	if !isHex(sha) {
		return false, fmt.Errorf("invalid sha %q", sha)
	}
	err := s.git("cat-file", "-e", sha).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if asExitErr(err, &exitErr) {
		return false, nil
	}
	return false, err
}

// GC runs `git gc` against the blob repo. Blobs unreachable longer than the
// prune-expire window (31 days by default) are reclaimed.
func (s *Store) GC() error {
	if out, err := s.git("gc", "--quiet").CombinedOutput(); err != nil {
		return fmt.Errorf("git gc: %w: %s", err, out)
	}
	return nil
}

func (s *Store) git(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+s.gitDir)
	return cmd
}

// IsBinary reports whether the first 8 KiB of buf contain a null byte,
// the classic heuristic used by git, diff, and grep.
func IsBinary(buf []byte) bool {
	const probe = 8 << 10
	if len(buf) > probe {
		buf = buf[:probe]
	}
	return bytes.IndexByte(buf, 0) >= 0
}

func isHex(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func asExitErr(err error, out **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*out = ee
	}
	return ok
}
