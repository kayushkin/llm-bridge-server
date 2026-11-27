package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// A harness whose logo file is not in the images directory gets no address,
// so the page draws its emoji rather than a broken picture and a 404.
func TestHarnessImageURLIsEmptyWhenTheFileIsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "harnesses"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harnesses", "claude_code.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &config.Config{ImagesDir: dir}}

	if got := srv.harnessImageURL(msg.HarnessClaudeCode, "claude_code.png"); !strings.HasPrefix(got, "/images/harnesses/claude_code.png?v=") {
		t.Fatalf("a present logo: got %q, want its address with a version", got)
	}
	if got := srv.harnessImageURL(msg.HarnessCodex, "codex.svg"); got != "" {
		t.Fatalf("a missing logo: got %q, want no address", got)
	}
	if got := srv.harnessImageURL(msg.HarnessCodex, ""); got != "" {
		t.Fatalf("a harness with no logo: got %q, want no address", got)
	}
}
