package repostore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRenamable registers "producer" with one deploy on the ledger, plus a
// second repo "dash" whose name and path the refusal tests try to take.
func seedRenamable(t *testing.T, s *Store) (*Repo, *Repo) {
	t.Helper()
	producer := &Repo{Name: "producer", Path: "/old/producer", GitRemote: "git@gh-alias:example-owner/producer.git", Tags: []string{"go"}}
	if _, err := s.UpsertRepo(producer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProvenance(producer.ID, ProvenanceOurs); err != nil {
		t.Fatal(err)
	}
	d := &Deployment{CommitSHA: strings.Repeat("a", 40), Branch: "main", DefaultBranch: "main", SourcePath: "/old/producer", OnDefault: true}
	if err := s.RecordDeployment(producer.ID, d); err != nil {
		t.Fatal(err)
	}
	dash := &Repo{Name: "dash", Path: "/x/dash"}
	if _, err := s.UpsertRepo(dash); err != nil {
		t.Fatal(err)
	}
	return producer, dash
}

func identityBody(name, path, remote string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "path": path, "git_remote": remote})
	return string(b)
}

func TestPutIdentityRenamesAndMovesTheRowAndKeepsItsId(t *testing.T) {
	srv, s := testServer(t)
	producer, _ := seedRenamable(t, s)

	// The new path does not exist yet: the stored tags stay.
	status, body := do(t, "PUT", fmt.Sprintf("%s/repos/%d/identity", srv.URL, producer.ID),
		identityBody("orchestrator", "/new/orchestrator", "git@gh-alias:example-owner/orchestrator.git"))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var got Repo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != producer.ID || got.Name != "orchestrator" || got.Path != "/new/orchestrator" || got.GitRemote != "git@gh-alias:example-owner/orchestrator.git" {
		t.Fatalf("identity not applied: %+v", got)
	}
	if got.Provenance != ProvenanceOurs || !hasTag(got.Tags, "go") {
		t.Fatalf("provenance or tags changed: %+v", got)
	}

	if r, err := s.GetRepoByName("orchestrator"); err != nil || r.ID != producer.ID {
		t.Fatalf("by name: %v %+v", err, r)
	}
	if r, err := s.GetRepoByPath("/new/orchestrator"); err != nil || r.ID != producer.ID {
		t.Fatalf("by path: %v %+v", err, r)
	}
	if _, err := s.GetRepoByName("producer"); err != ErrNotFound {
		t.Fatalf("old name still answers: %v", err)
	}
	deployments, err := s.ListDeployments(producer.ID, 0)
	if err != nil || len(deployments) != 1 {
		t.Fatalf("ledger lost its row: %v %+v", err, deployments)
	}
	if deployments[0].SourcePath != "/old/producer" {
		t.Fatalf("ledger history was rewritten: %q", deployments[0].SourcePath)
	}
}

func TestPutIdentityRedetectsWhenTheNewPathExists(t *testing.T) {
	srv, s := testServer(t)
	producer, _ := seedRenamable(t, s)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, body := do(t, "PUT", fmt.Sprintf("%s/repos/%d/identity", srv.URL, producer.ID), identityBody("orchestrator", dir, ""))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	got, _ := s.GetRepo(producer.ID)
	if !hasTag(got.Tags, "rust") || hasTag(got.Tags, "go") {
		t.Fatalf("tags not re-detected from the new path: %v", got.Tags)
	}
	if got.GitRemote != "" {
		t.Fatalf("an empty git_remote was not stored: %q", got.GitRemote)
	}
}

func TestPutIdentityRefusals(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"name held by another repo", identityBody("dash", "/new/orchestrator", ""), 409},
		{"path held by another repo", identityBody("orchestrator", "/x/dash", ""), 409},
		{"empty name", identityBody("", "/new/orchestrator", ""), 400},
		{"relative path", identityBody("orchestrator", "repos/orchestrator", ""), 400},
		{"empty path", identityBody("orchestrator", "", ""), 400},
		{"missing git_remote", `{"name":"orchestrator","path":"/new/orchestrator"}`, 400},
		{"missing name", `{"path":"/new/orchestrator","git_remote":""}`, 400},
		{"unknown field", `{"name":"orchestrator","path":"/new/orchestrator","git_remote":"","provenance":"ours"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, s := testServer(t)
			producer, _ := seedRenamable(t, s)
			status, body := do(t, "PUT", fmt.Sprintf("%s/repos/%d/identity", srv.URL, producer.ID), c.body)
			if status != c.status {
				t.Fatalf("status %d, want %d: %s", status, c.status, body)
			}
			got, _ := s.GetRepo(producer.ID)
			if got.Name != "producer" || got.Path != "/old/producer" || got.GitRemote != "git@gh-alias:example-owner/producer.git" {
				t.Fatalf("a refused request changed the row: %+v", got)
			}
		})
	}
}

func TestPutIdentityOnAMissingRepoIsNotFound(t *testing.T) {
	srv, _ := testServer(t)
	status, body := do(t, "PUT", srv.URL+"/repos/999/identity", identityBody("orchestrator", "/new/orchestrator", ""))
	if status != 404 {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestPutIdentityKeepingTheSameNameAndPathIsAllowed(t *testing.T) {
	srv, s := testServer(t)
	producer, _ := seedRenamable(t, s)
	status, body := do(t, "PUT", fmt.Sprintf("%s/repos/%d/identity", srv.URL, producer.ID),
		identityBody("producer", "/old/producer", "git@gh-alias:example-owner/orchestrator.git"))
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
}
