package localtools

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"
)

// findRecentlyModifiedGit fails in two ways that its own caller cannot tell
// apart, and that is the point: findRecentlyModified asks only whether an
// error came back, then consults ctx.Err() rather than the error it was given.
// So nothing downstream pins what either failure says, and both were free to
// become any other error without a single test noticing.
//
// The distinction is load-bearing one level up. "This is not a git
// repository" is the case the mtime walk exists to serve, and "the caller
// canceled" is the case it must not serve — falling back there would start
// the expensive scan that the cancellation was asking to avoid.
func TestTheGitStrategyReportsADirectoryWithNoRepositoryAsAMissingFile(t *testing.T) {
	_, err := findRecentlyModifiedGit(context.Background(), t.TempDir(), time.Hour)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want a missing-.git error, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Error("a directory that is simply not a repository was reported as a cancellation")
	}
}
