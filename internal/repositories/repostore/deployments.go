package repostore

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Deployment is one deploy of a repo, as the shared deploy gate recorded it.
// Rows are append-only: a deploy that happened cannot un-happen, and a ledger
// that can be edited cannot settle an argument about what went out.
type Deployment struct {
	ID             int64    `json:"id"`
	RepoID         int64    `json:"repo_id"`
	RepoName       string   `json:"repo_name,omitempty"` // display only; join on RepoID
	CommitSHA      string   `json:"commit_sha"`
	Branch         string   `json:"branch"`
	DefaultBranch  string   `json:"default_branch"`
	SourcePath     string   `json:"source_path"`
	OnDefault      bool     `json:"on_default"`
	Dirty          bool     `json:"dirty"`
	OverrideReason string   `json:"override_reason,omitempty"`
	Refusals       []string `json:"refusals"`
	DeployedBy     string   `json:"deployed_by,omitempty"`
	CreatedAt      int64    `json:"created_at"`
}

// Clean reports whether this deploy is what a deploy should be: the gate had
// nothing to object to.
func (d *Deployment) Clean() bool { return len(d.Refusals) == 0 }

var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validateDeployment(d *Deployment) error {
	d.CommitSHA = strings.TrimSpace(d.CommitSHA)
	if !fullCommitSHA.MatchString(d.CommitSHA) {
		return fmt.Errorf("commit_sha must be the full 40-character sha, got %q: a short sha stops being unique as the repo grows", d.CommitSHA)
	}
	if strings.TrimSpace(d.DefaultBranch) == "" {
		return errors.New("default_branch is required")
	}
	if strings.TrimSpace(d.SourcePath) == "" {
		return errors.New("source_path is required")
	}
	d.OverrideReason = strings.TrimSpace(d.OverrideReason)
	if d.Refusals == nil {
		d.Refusals = []string{}
	}
	// The two must agree: an override with nothing refused is a reason for
	// nothing, and a refused deploy with no override did not happen.
	if len(d.Refusals) > 0 && d.OverrideReason == "" {
		return errors.New("refusals are listed but override_reason is empty: a deploy the gate refused only happens through an override, and the override needs its reason")
	}
	if len(d.Refusals) == 0 && d.OverrideReason != "" {
		return errors.New("override_reason is set but nothing was refused")
	}
	return nil
}

// RecordDeployment appends one deployment for repoID.
func (s *Store) RecordDeployment(repoID int64, d *Deployment) error {
	if _, err := s.GetRepo(repoID); err != nil {
		return err
	}
	if err := validateDeployment(d); err != nil {
		return err
	}
	refusals, err := json.Marshal(d.Refusals)
	if err != nil {
		return err
	}
	d.RepoID, d.CreatedAt = repoID, now()
	res, err := s.db.Exec(`
		INSERT INTO deployments (repo_id, commit_sha, branch, default_branch, source_path, on_default, dirty, override_reason, refusals, deployed_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, d.CommitSHA, d.Branch, d.DefaultBranch, d.SourcePath, boolInt(d.OnDefault), boolInt(d.Dirty), d.OverrideReason, string(refusals), d.DeployedBy, d.CreatedAt)
	if err != nil {
		return err
	}
	d.ID, err = res.LastInsertId()
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const deploymentColumns = `d.id, d.repo_id, r.name, d.commit_sha, d.branch, d.default_branch, d.source_path, d.on_default, d.dirty, d.override_reason, d.refusals, d.deployed_by, d.created_at`

type deploymentScanner interface{ Scan(dest ...any) error }

func scanDeployment(row deploymentScanner) (*Deployment, error) {
	var d Deployment
	var onDefault, dirty int
	var refusals string
	if err := row.Scan(&d.ID, &d.RepoID, &d.RepoName, &d.CommitSHA, &d.Branch, &d.DefaultBranch, &d.SourcePath, &onDefault, &dirty, &d.OverrideReason, &refusals, &d.DeployedBy, &d.CreatedAt); err != nil {
		return nil, err
	}
	d.OnDefault, d.Dirty = onDefault == 1, dirty == 1
	d.Refusals = []string{}
	if err := json.Unmarshal([]byte(refusals), &d.Refusals); err != nil {
		return nil, fmt.Errorf("deployment %d refusals: %w", d.ID, err)
	}
	return &d, nil
}

// ListDeployments returns a repo's deployments, newest first.
func (s *Store) ListDeployments(repoID int64, limit int) ([]Deployment, error) {
	if _, err := s.GetRepo(repoID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+deploymentColumns+` FROM deployments d JOIN repos r ON r.id=d.repo_id WHERE d.repo_id=? ORDER BY d.id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDeployments(rows)
}

// LatestDeployments returns the newest deployment of every repo that has one.
func (s *Store) LatestDeployments() ([]Deployment, error) {
	rows, err := s.db.Query(`SELECT ` + deploymentColumns + ` FROM deployments d JOIN repos r ON r.id=d.repo_id
		WHERE d.id IN (SELECT MAX(id) FROM deployments GROUP BY repo_id) ORDER BY r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDeployments(rows)
}

type deploymentRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func collectDeployments(rows deploymentRows) ([]Deployment, error) {
	out := []Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}
