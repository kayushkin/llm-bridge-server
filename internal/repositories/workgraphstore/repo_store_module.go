package workgraphstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
)

// RepoStoreModule is RepoDirectory over the repo-store module in this
// process. It answers what repo-store's routes answered over HTTP: the same
// repos, with github_url derived from the remote as repo-store derives it.
type RepoStoreModule struct {
	Store *repostore.Store
}

func repoOf(repo repostore.Repo) Repo {
	return Repo{ID: repo.ID, Name: repo.Name, Path: repo.Path, GitHubURL: repo.GitHubURL}
}

func (module RepoStoreModule) Repos(context.Context) ([]Repo, error) {
	repos, err := module.Store.ListRepos(repostore.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("repo-store: listing repos: %w", err)
	}
	listed := make([]Repo, 0, len(repos))
	for _, repo := range repos {
		listed = append(listed, repoOf(repo))
	}
	return listed, nil
}

func (module RepoStoreModule) Repo(_ context.Context, id int64) (Repo, error) {
	repo, err := module.Store.GetRepo(id)
	if errors.Is(err, repostore.ErrNotFound) {
		return Repo{}, fmt.Errorf("repo-store has no repo %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Repo{}, fmt.Errorf("repo-store: repo %d: %w", id, err)
	}
	return repoOf(*repo), nil
}

func (module RepoStoreModule) RepoByName(_ context.Context, name string) (Repo, error) {
	repo, err := module.Store.GetRepoByName(name)
	if errors.Is(err, repostore.ErrNotFound) {
		return Repo{}, fmt.Errorf("repo-store has no repo named %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Repo{}, fmt.Errorf("repo-store: repo %q: %w", name, err)
	}
	return repoOf(*repo), nil
}
