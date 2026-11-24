package repodetect

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasTag(tags []string, want string) bool {
	for _, x := range tags {
		if x == want {
			return true
		}
	}
	return false
}

func TestDetectGo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module x\n")
	sig, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTag(sig.Languages, "go") {
		t.Fatalf("want go, got %v", sig.Languages)
	}
}

// TestDetectFullStack is the case inber's first-match detector gets wrong: a Go
// service with a React frontend must report both languages plus the frontend
// signature.
func TestDetectFullStack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module x\n")
	writeFile(t, dir, "package.json", `{"dependencies":{"react":"19"},"devDependencies":{"vite":"8","@playwright/test":"1","typescript":"5"}}`)
	sig, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	tags := sig.Tags()
	for _, want := range []string{"go", "node", "react", "vite", "typescript", "e2e", "playwright", "web", "frontend"} {
		if !hasTag(tags, want) {
			t.Fatalf("want %q in %v", want, tags)
		}
	}
}

func TestDetectViteConfigFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"dependencies":{"react":"19"}}`)
	writeFile(t, dir, "vite.config.ts", "export default {}")
	sig, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTag(sig.BuildTools, "vite") {
		t.Fatalf("vite via config file not detected: %v", sig.BuildTools)
	}
}

func TestDetectRustPython(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Cargo.toml", "[package]\n")
	writeFile(t, dir, "requirements.txt", "flask\n")
	sig, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTag(sig.Languages, "rust") || !hasTag(sig.Languages, "python") {
		t.Fatalf("want rust+python, got %v", sig.Languages)
	}
}

func TestDetectMissingPath(t *testing.T) {
	if _, err := Detect("/nonexistent/path/xyz-123"); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestTagsRollup(t *testing.T) {
	sig := Signature{Frameworks: []string{"vue"}}
	tags := sig.Tags()
	if !hasTag(tags, "web") || !hasTag(tags, "frontend") {
		t.Fatalf("vue should roll up to web/frontend: %v", tags)
	}
}

func TestPrimaryLanguage(t *testing.T) {
	// go wins over node when both present (command-selection preference order).
	if got := (Signature{Languages: []string{"node", "go"}}).PrimaryLanguage(); got != "go" {
		t.Fatalf("want go primary, got %q", got)
	}
	if got := (Signature{Languages: []string{"node", "typescript"}}).PrimaryLanguage(); got != "node" {
		t.Fatalf("want node primary, got %q", got)
	}
	if got := (Signature{}).PrimaryLanguage(); got != "" {
		t.Fatalf("want empty primary for empty signature, got %q", got)
	}
}
