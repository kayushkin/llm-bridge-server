package gitactivity

import (
	"context"
	"fmt"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}

// fakeRepoDirectory is repo-store holding repos.
type fakeRepoDirectory struct{ repos []Repo }

func (directory fakeRepoDirectory) Repos(context.Context) ([]Repo, error) {
	return directory.repos, nil
}

func (directory fakeRepoDirectory) Repo(_ context.Context, id int64) (Repo, error) {
	for _, repo := range directory.repos {
		if repo.ID == id {
			return repo, nil
		}
	}
	return Repo{}, fmt.Errorf("no repo %d: %w", id, ErrNotFound)
}

func (directory fakeRepoDirectory) RepoByName(_ context.Context, name string) (Repo, error) {
	for _, repo := range directory.repos {
		if repo.Name == name {
			return repo, nil
		}
	}
	return Repo{}, fmt.Errorf("no repo %q: %w", name, ErrNotFound)
}
