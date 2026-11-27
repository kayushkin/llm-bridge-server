package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/issuesclient"
)

func repoLink(ref string) issuesclient.CardLink {
	return issuesclient.CardLink{EntityType: "repo", EntityRef: ref}
}

func TestACardWithNoRepoLinkLeavesTheWorkingDirToTheInstance(t *testing.T) {
	got, err := repoWorkingDir([]issuesclient.CardLink{
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
	got, err := repoWorkingDir([]issuesclient.CardLink{repoLink(dir)})
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
	_, err := repoWorkingDir([]issuesclient.CardLink{repoLink(a), repoLink(b)})
	if err == nil {
		t.Fatal("a card with two repo links was given one of them")
	}
	if !strings.Contains(err.Error(), "2 repo links") {
		t.Fatalf("error does not say what is wrong: %v", err)
	}
}

func TestAPathThatIsNotThereIsRefused(t *testing.T) {
	if _, err := repoWorkingDir([]issuesclient.CardLink{repoLink("/nowhere/at/all")}); err == nil {
		t.Fatal("a repo link to a missing path was accepted")
	}
}

func TestAPathThatIsAFileIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-repo")
	if err := writeFileForTest(file); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := repoWorkingDir([]issuesclient.CardLink{repoLink(file)}); err == nil {
		t.Fatal("a repo link to a plain file was accepted")
	}
}

func TestAnEmptyRefIsIgnoredRatherThanUsed(t *testing.T) {
	got, err := repoWorkingDir([]issuesclient.CardLink{repoLink("   ")})
	if err != nil {
		t.Fatalf("repoWorkingDir: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func agentLink(ref string) issuesclient.CardLink {
	return issuesclient.CardLink{EntityType: "agent", EntityRef: ref}
}

func TestACardWithNoAgentLinkStaysUnattributed(t *testing.T) {
	got, err := sessionAgentSlug([]issuesclient.CardLink{repoLink("/tmp")})
	if err != nil {
		t.Fatalf("sessionAgentSlug: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty — that is what every session has been until now", got)
	}
}

func TestAnAgentLinkBecomesTheSessionIdentity(t *testing.T) {
	got, err := sessionAgentSlug([]issuesclient.CardLink{agentLink("northwind-builder")})
	if err != nil {
		t.Fatalf("sessionAgentSlug: %v", err)
	}
	if got != "northwind-builder" {
		t.Fatalf("got %q, want northwind-builder", got)
	}
}

// A card naming two identities does not say which one did the work, and
// attribution that picks one is worse than none.
func TestTwoAgentLinksAreRefused(t *testing.T) {
	_, err := sessionAgentSlug([]issuesclient.CardLink{
		agentLink("northwind-builder"), agentLink("northwind-reviewer"),
	})
	if err == nil {
		t.Fatal("a card claiming two identities was given one of them")
	}
	if !strings.Contains(err.Error(), "2 agent links") {
		t.Fatalf("error does not say what is wrong: %v", err)
	}
}

func TestTheTwoLinkKindsAreReadIndependently(t *testing.T) {
	links := []issuesclient.CardLink{
		agentLink("northwind-reviewer"),
		repoLink(t.TempDir()),
		{EntityType: "session", EntityRef: "autoworker-x-1"},
	}
	slug, err := sessionAgentSlug(links)
	if err != nil {
		t.Fatalf("sessionAgentSlug: %v", err)
	}
	dir, err := repoWorkingDir(links)
	if err != nil {
		t.Fatalf("repoWorkingDir: %v", err)
	}
	if slug != "northwind-reviewer" || dir == "" {
		t.Fatalf("slug=%q dir=%q — a card carries both without either reading the other", slug, dir)
	}
}
