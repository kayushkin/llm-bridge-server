package repostore

import (
	"encoding/json"
	"errors"
	"testing"
)

// withSSHHosts replaces ssh -G with a fixed alias table for one test.
func withSSHHosts(t *testing.T, hosts map[string]string) {
	t.Helper()
	previous := sshHostResolver
	sshHostResolver = func(host string) (string, error) {
		if resolved, ok := hosts[host]; ok {
			return resolved, nil
		}
		return "", errors.New("unknown host " + host)
	}
	t.Cleanup(func() { sshHostResolver = previous })
}

func TestGitHubURLOfRemoteReadsEveryRemoteFormAndResolvesSSHAliases(t *testing.T) {
	withSSHHosts(t, map[string]string{"gh-alias": "github.com", "github.com": "github.com", "gitlab.com": "gitlab.com"})
	cases := map[string]string{
		"git@gh-alias:example-owner/dash.git":          "https://github.com/example-owner/dash",
		"git@github.com:example-owner/repo-store.git":  "https://github.com/example-owner/repo-store",
		"https://github.com/example-owner/example.com": "https://github.com/example-owner/example.com",
		"https://github.com/owner/name.git":            "https://github.com/owner/name",
		"ssh://git@gh-alias/owner/name.git":            "https://github.com/owner/name",
		"git@gitlab.com:owner/name.git":                "",
		"https://gitlab.com/owner/name":                "",
		"git@nowhere:owner/name.git":                   "", // alias ssh cannot resolve
		"":                                             "",
		"/srv/mirrors/some-local-mirror":               "",
	}
	for remote, want := range cases {
		if got := GitHubURLOfRemote(remote); got != want {
			t.Errorf("GitHubURLOfRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestARepoReadBackCarriesItsGitHubURL(t *testing.T) {
	withSSHHosts(t, map[string]string{"gh-alias": "github.com"})
	s := openTest(t)
	if _, err := s.UpsertRepo(&Repo{Name: "dash", GitRemote: "git@gh-alias:example-owner/dash.git"}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.GetRepoByName("dash")
	if err != nil {
		t.Fatal(err)
	}
	if repo.GitHubURL != "https://github.com/example-owner/dash" {
		t.Fatalf("github_url = %q", repo.GitHubURL)
	}
}

func TestByMentionAnswersARepoMentionAndRefusesAnythingElse(t *testing.T) {
	withSSHHosts(t, map[string]string{"gh-alias": "github.com"})
	srv, s := testServer(t)
	if _, err := s.UpsertRepo(&Repo{Name: "dash", GitRemote: "git@gh-alias:example-owner/dash.git"}); err != nil {
		t.Fatal(err)
	}

	status, body := do(t, "GET", srv.URL+"/repos/by-mention/repo:dash", "")
	if status != 200 {
		t.Fatalf("repo:dash: %d %s", status, body)
	}
	var repo Repo
	if err := json.Unmarshal(body, &repo); err != nil {
		t.Fatal(err)
	}
	if repo.Name != "dash" || repo.GitHubURL != "https://github.com/example-owner/dash" {
		t.Fatalf("repo:dash answered %+v", repo)
	}

	if status, _ := do(t, "GET", srv.URL+"/repos/by-mention/Repo:dash", ""); status != 200 {
		t.Errorf("Repo:dash (the matcher is case-insensitive) = %d, want 200", status)
	}
	if status, _ := do(t, "GET", srv.URL+"/repos/by-mention/repo:nope", ""); status != 404 {
		t.Errorf("repo:nope = %d, want 404", status)
	}
	if status, _ := do(t, "GET", srv.URL+"/repos/by-mention/dash", ""); status != 400 {
		t.Errorf("a bare name = %d, want 400", status)
	}
}
