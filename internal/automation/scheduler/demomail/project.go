package demomail

import (
	"encoding/json"
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Project is what a colleague would know about a repository before writing to
// somebody about it.
//
// Read from the repository itself rather than described by hand. The first
// version of this demo used hand-written card titles, and one of them asked an
// agent to "bump the build image to Node 22 LTS" in a repository with no Node
// in it anywhere. The agent checked every branch and all twelve commits,
// refused, and wrote down why — correct behavior, and a wasted session. Mail
// written from the actual code cannot ask for that.
type Project struct {
	Name     string
	Path     string
	Remote   string
	Readme   string
	Packages []string
	// Endpoints are the HTTP routes the service actually serves, so a request
	// can name one that exists.
	Endpoints []string
	Commits   []string
	OpenPRs   []string
	// Files is a shallow listing, enough to name a real file in a request.
	Files []string
	Tests int
}

// ReadProject inspects a repository.
//
// Every step is best-effort EXCEPT the repository itself: a path that is not a
// git repository is an error, because everything below would otherwise return
// empty and the model would be asked to write about a project it knows nothing
// about — which is exactly how the Node request happened.
func ReadProject(path string) (*Project, error) {
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a git repository, so there is nothing to write about", path)
	}
	p := &Project{Name: filepath.Base(path), Path: path}

	if readme, err := os.ReadFile(filepath.Join(path, "README.md")); err == nil {
		p.Readme = string(readme)
	}
	p.Remote = strings.TrimSpace(run(path, "git", "remote", "get-url", "origin"))
	p.Commits = lines(run(path, "git", "log", "--oneline", "-12"))
	p.Packages = subdirs(filepath.Join(path, "internal"))
	p.Files = lines(run(path, "git", "ls-files"))
	p.Endpoints = endpoints(path)
	if p.OpenPRs, err = openPullRequests(path); err != nil {
		return nil, err
	}
	p.Tests = countTests(run(path, "git", "grep", "-c", "^func Test", "--", "*_test.go"))

	if p.Readme == "" && len(p.Files) == 0 {
		return nil, fmt.Errorf("%s has no README and no tracked files; there is nothing to write a request about", path)
	}
	return p, nil
}

// endpoints reads the routes the service registers. Grep rather than parse: the
// point is to name real paths in an email, not to build a router.
func endpoints(path string) []string {
	out := run(path, "git", "grep", "-hoP", `HandleFunc\("\K[^"]+`)
	seen := map[string]bool{}
	var routes []string
	for _, r := range lines(out) {
		if r != "" && !seen[r] {
			seen[r] = true
			routes = append(routes, r)
		}
	}
	return routes
}

// openPullRequests asks gh. A missing gh, or one that cannot list, is an
// error rather than an empty list: run() swallowing it is how scheduler job 82
// told the model "0 open PR(s)" for a repository with three, because systemd's
// PATH does not include ~/bin. Mail that references nothing real is the
// failure this whole reader exists to prevent, so it must not be silent. A
// repository with genuinely no open pull requests still returns an empty list.
func openPullRequests(path string) ([]string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh is not on PATH (%s), so open pull requests cannot be read: %w", os.Getenv("PATH"), err)
	}
	cmd := exec.Command("gh", "pr", "list", "--state", "open", "--json", "number,title,headRefName")
	cmd.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = ": " + strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("gh pr list in %s: %w%s", path, err, detail)
	}
	var prs []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list returned something that is not JSON: %w", err)
	}
	var described []string
	for _, pr := range prs {
		described = append(described, fmt.Sprintf("#%d %s (branch %s)", pr.Number, pr.Title, pr.HeadRefName))
	}
	return described, nil
}

func countTests(grepOutput string) int {
	total := 0
	for _, l := range lines(grepOutput) {
		if i := strings.LastIndex(l, ":"); i >= 0 {
			var n int
			if _, err := fmt.Sscanf(l[i+1:], "%d", &n); err == nil {
				total += n
			}
		}
	}
	return total
}

func subdirs(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// run executes a command in the repository and returns its output, or empty on
// failure. Best-effort by design: a missing gh or a repository with no commits
// yet should narrow what the model is told, not stop the run.
func run(dir string, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Brief renders the project for a prompt, bounded so one repository cannot
// crowd out the instructions around it.
func (p *Project) Brief() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PROJECT: %s\n", p.Name)
	if p.Remote != "" {
		fmt.Fprintf(&b, "remote: %s\n", p.Remote)
	}
	if len(p.Packages) > 0 {
		fmt.Fprintf(&b, "packages under internal/: %s\n", strings.Join(p.Packages, ", "))
	}
	if len(p.Endpoints) > 0 {
		fmt.Fprintf(&b, "HTTP routes it serves: %s\n", strings.Join(p.Endpoints, ", "))
	}
	fmt.Fprintf(&b, "tests: %d\n", p.Tests)
	if len(p.OpenPRs) > 0 {
		fmt.Fprintf(&b, "open pull requests:\n  %s\n", strings.Join(p.OpenPRs, "\n  "))
	}
	if len(p.Commits) > 0 {
		fmt.Fprintf(&b, "recent commits:\n  %s\n", strings.Join(p.Commits, "\n  "))
	}
	if len(p.Files) > 0 {
		fmt.Fprintf(&b, "files:\n  %s\n", strings.Join(clamp(p.Files, 40), "\n  "))
	}
	if p.Readme != "" {
		fmt.Fprintf(&b, "\nREADME:\n%s\n", clampText(p.Readme, 3000))
	}
	return b.String()
}

func clamp(v []string, n int) []string {
	if len(v) <= n {
		return v
	}
	return append(append([]string{}, v[:n]...), fmt.Sprintf("… and %d more", len(v)-n))
}

func clampText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…"
}
