package repostore

import (
	"strings"
	"testing"
)

const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func deployableRepo(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	r := &Repo{Name: name, Path: "/repos/" + name}
	if _, err := s.UpsertRepo(r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestDeploymentsAreRecordedNewestFirstAndLatestIsPerRepo(t *testing.T) {
	s := openTest(t)
	dash, bus := deployableRepo(t, s, "dash"), deployableRepo(t, s, "bus")
	for _, d := range []struct {
		repo int64
		sha  string
	}{{dash, shaA}, {bus, shaA}, {dash, shaB}} {
		if err := s.RecordDeployment(d.repo, &Deployment{CommitSHA: d.sha, Branch: "main", DefaultBranch: "main", SourcePath: "/repos/x", OnDefault: true}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.ListDeployments(dash, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].CommitSHA != shaB || !history[0].Clean() {
		t.Fatalf("dash history = %+v, want 2 rows newest first", history)
	}
	latest, err := s.LatestDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 || latest[0].RepoName != "bus" || latest[1].CommitSHA != shaB {
		t.Fatalf("latest = %+v, want one row per repo with dash at its second deploy", latest)
	}
}

func TestADeployTheGateRefusedNeedsItsReasonAndAReasonNeedsARefusal(t *testing.T) {
	s := openTest(t)
	id := deployableRepo(t, s, "dash")
	base := Deployment{CommitSHA: shaA, Branch: "feat/x", DefaultBranch: "main", SourcePath: "/repos/dash-wt-x"}

	refusedNoReason := base
	refusedNoReason.Refusals = []string{"not on the default branch"}
	if err := s.RecordDeployment(id, &refusedNoReason); err == nil {
		t.Fatal("recorded a refused deploy with no override reason")
	}
	reasonNoRefusal := base
	reasonNoRefusal.OverrideReason = "hotfix"
	if err := s.RecordDeployment(id, &reasonNoRefusal); err == nil {
		t.Fatal("recorded an override reason with nothing refused")
	}
	overridden := base
	overridden.Refusals, overridden.OverrideReason = []string{"not on the default branch"}, "hotfix while main is red"
	if err := s.RecordDeployment(id, &overridden); err != nil {
		t.Fatal(err)
	}
	history, _ := s.ListDeployments(id, 0)
	if len(history) != 1 || history[0].Clean() || history[0].OverrideReason != "hotfix while main is red" {
		t.Fatalf("history = %+v", history)
	}
}

func TestDeploymentRefusesAShortShaAndAnUnknownRepo(t *testing.T) {
	s := openTest(t)
	id := deployableRepo(t, s, "dash")
	err := s.RecordDeployment(id, &Deployment{CommitSHA: "abc1234", DefaultBranch: "main", SourcePath: "/x"})
	if err == nil || !strings.Contains(err.Error(), "40-character") {
		t.Fatalf("short sha: err = %v", err)
	}
	if err := s.RecordDeployment(9999, &Deployment{CommitSHA: shaA, DefaultBranch: "main", SourcePath: "/x"}); err == nil {
		t.Fatal("recorded a deployment for a repo that does not exist")
	}
}
