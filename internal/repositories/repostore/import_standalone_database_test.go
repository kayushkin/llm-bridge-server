package repostore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"

	_ "modernc.org/sqlite"
)

// writeStandaloneDatabase makes a SQLite file as the standalone repo-store
// wrote it, holding statements' rows.
func writeStandaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo-store.db")
	source, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	schema, err := os.ReadFile("testdata/standalone_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	for _, statement := range statements {
		if _, err := source.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func newImportTarget(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	return db, New(db)
}

const sha40 = "0123456789abcdef0123456789abcdef01234567"

func TestImportKeepsEveryRowAndItsID(t *testing.T) {
	path := writeStandaloneDatabase(t,
		`INSERT INTO repos (name, path, git_remote, description, provenance, tags, tag_overrides, created_at, updated_at)
			VALUES ('webapp', '/srv/src/webapp', 'git@github.com:example-owner/webapp.git', 'the site', 'ours', '["go","react"]', '["primary"]', 10, 20)`,
		`INSERT INTO repos (name, path, created_at, updated_at) VALUES ('gone', '/srv/src/gone', 11, 11)`,
		`INSERT INTO repos (name, path, created_at, updated_at) VALUES ('tools', '/srv/src/tools', 12, 12)`,
		`DELETE FROM repos WHERE name = 'gone'`,
		`INSERT INTO deployments (repo_id, commit_sha, branch, default_branch, source_path, on_default, dirty, override_reason, refusals, deployed_by, created_at)
			VALUES (1, '`+sha40+`', 'main', 'main', '/srv/src/webapp', 1, 0, '', '[]', 'agent session one', 30)`,
		`INSERT INTO deployments (repo_id, commit_sha, branch, default_branch, source_path, on_default, dirty, override_reason, refusals, deployed_by, created_at)
			VALUES (3, '`+sha40+`', 'fix', 'main', '/srv/src/tools-wt', 0, 1, 'an emergency', '["not on the default branch","dirty tree"]', '', 31)`,
		`INSERT INTO pull_requests VALUES (1, 7, 'Add a page', 'merged', 0, 'someone', 'page', 'main', '`+sha40+`', 'https://github.com/example-owner/webapp/pull/7', 40, 41, 42, 42, 43)`,
		`INSERT INTO pull_request_syncs VALUES (1, 'https://github.com/example-owner/webapp', 44, 45)`,
	)
	db, store := newImportTarget(t)
	counts, err := ImportStandaloneDatabase(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Repos != 2 || counts.Deployments != 2 || counts.PullRequests != 1 || counts.PullRequestSyncs != 1 || counts.LastRepoID != 3 || counts.LastDeploymentID != 2 {
		t.Fatalf("counts %+v", counts)
	}
	webapp, err := store.GetRepo(1)
	if err != nil {
		t.Fatal(err)
	}
	if webapp.Name != "webapp" || webapp.Provenance != ProvenanceOurs || webapp.GitHubURL != "https://github.com/example-owner/webapp" ||
		strings.Join(webapp.EffectiveTags, ",") != "go,primary,react" || webapp.CreatedAt != 10 || webapp.UpdatedAt != 20 {
		t.Fatalf("webapp came across as %+v", webapp)
	}
	if _, err := store.GetRepo(2); err != ErrNotFound {
		t.Fatalf("the deleted repo's id: %v", err)
	}
	deployments, err := store.ListDeployments(3, 10)
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments of tools: %v %+v", err, deployments)
	}
	if d := deployments[0]; d.OnDefault || !d.Dirty || d.OverrideReason != "an emergency" || len(d.Refusals) != 2 || d.RepoName != "tools" {
		t.Fatalf("deployment came across as %+v", d)
	}
	pullRequests, err := store.ListPullRequests(PullRequestListFilter{RepoID: 1, State: PullRequestStateQueryAll, Limit: 10})
	if err != nil || len(pullRequests) != 1 || pullRequests[0].State != PullRequestStateMerged || pullRequests[0].SyncedAt != 43 {
		t.Fatalf("pull requests: %v %+v", err, pullRequests)
	}
	cursor, found, err := store.PullRequestSyncCursorOf(1)
	if err != nil || !found || cursor.StartedAt != 44 || cursor.FinishedAt != 45 {
		t.Fatalf("cursor: %v %v %+v", err, found, cursor)
	}

	// Numbering carries on after the highest id ever handed out, so the
	// deleted repo's id 2 is not given to the next repo.
	next := &Repo{Name: "next"}
	if _, err := store.UpsertRepo(next); err != nil {
		t.Fatal(err)
	}
	if next.ID != 4 {
		t.Fatalf("the next repo is %d, want 4", next.ID)
	}
	if _, err := ImportStandaloneDatabase(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportRefusesAFileItCannotCopyWhole(t *testing.T) {
	cases := map[string]string{
		"an unknown column": `ALTER TABLE repos ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
		"tags that are not a list": `INSERT INTO repos (name, tags, created_at, updated_at) VALUES ('webapp', 'go,react', 1, 1)`,
		"an unknown provenance": `INSERT INTO repos (name, provenance, created_at, updated_at) VALUES ('webapp', 'borrowed', 1, 1)`,
	}
	for name, statement := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeStandaloneDatabase(t, statement)
			db, store := newImportTarget(t)
			if _, err := ImportStandaloneDatabase(context.Background(), db, path); err == nil {
				t.Fatal("imported")
			}
			if repos, err := store.ListRepos(ListFilter{}); err != nil || len(repos) != 0 {
				t.Fatalf("a refused import left %v %+v", err, repos)
			}
		})
	}
}
