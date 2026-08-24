package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

func repoLink(ref string) kanbanclient.CardLink {
	return kanbanclient.CardLink{EntityType: "repo", EntityRef: ref}
}

func TestACardWithNoRepoLinkLeavesTheWorkingDirToTheInstance(t *testing.T) {
	got, err := repoWorkingDir([]kanbanclient.CardLink{
		{EntityType: "session", EntityRef: "autoworker-x-1"},
		{EntityType: "email", EntityRef: "demo-work:lm1"},
	})
	if err != nil {
		t.Fatalf("repoWorkingDir: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty — the fleet's ordinary work is not scoped to a tree", got)
	}
}

func TestARepoLinkBecomesTheWorkingDir(t *testing.T) {
	dir := t.TempDir()
	got, err := repoWorkingDir([]kanbanclient.CardLink{repoLink(dir)})
	if err != nil {
		t.Fatalf("repoWorkingDir: %v", err)
	}
	if got != dir {
		t.Fatalf("got %q, want %q", got, dir)
	}
}

// Two repo links do not say which tree the work is in. Picking one would put an
// agent in an arbitrary repo holding a prompt telling it to change things.
func TestTwoRepoLinksAreRefusedRatherThanPicked(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	_, err := repoWorkingDir([]kanbanclient.CardLink{repoLink(a), repoLink(b)})
	if err == nil {
		t.Fatal("a card with two repo links was given one of them")
	}
	if !strings.Contains(err.Error(), "2 repo links") {
		t.Fatalf("error does not say what is wrong: %v", err)
	}
}

func TestAPathThatIsNotThereIsRefused(t *testing.T) {
	if _, err := repoWorkingDir([]kanbanclient.CardLink{repoLink("/nowhere/at/all")}); err == nil {
		t.Fatal("a repo link to a missing path was accepted")
	}
}

func TestAPathThatIsAFileIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-repo")
	if err := writeFileForTest(file); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := repoWorkingDir([]kanbanclient.CardLink{repoLink(file)}); err == nil {
		t.Fatal("a repo link to a plain file was accepted")
	}
}

func TestAnEmptyRefIsIgnoredRatherThanUsed(t *testing.T) {
	got, err := repoWorkingDir([]kanbanclient.CardLink{repoLink("   ")})
	if err != nil {
		t.Fatalf("repoWorkingDir: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
