//go:build harness_live

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The live harness tests drive the real `claude` and `codex` CLIs, and an
// out-of-date CLI breaks them in a way that has already cost something: it
// opens on a blocking update screen, and the line a test types answers it (on
// 2026-10-20 that ran Codex's own self-update). The scheduler keeps both CLIs
// current every night with scripts/update-harness-clis.sh. This test checks
// that job against the production scheduler: it exists, it is switched on, it
// runs that script every day, and its last run, if any, passed.

// nightlyHarnessCLIUpdateJobID is the scheduler's id for the job, which the
// scheduler assigned when the job was created on 2026-10-20. A job found by
// its name could be any job that happens to share it.
const nightlyHarnessCLIUpdateJobID = 107

// harnessCLIUpdateScript is the script the job must run, relative to the repo
// root.
const harnessCLIUpdateScript = "scripts/update-harness-clis.sh"

type schedulerJob struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Type     string `json:"type"`
	Command  string `json:"command"`
	Enabled  bool   `json:"enabled"`
}

type schedulerRun struct {
	ID         int       `json:"id"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Output     string    `json:"output"`
}

func TestHarnessCLIUpdateJobLive(t *testing.T) {
	schedulerURL := productionSchedulerURL(t)

	var job schedulerJob
	getSchedulerJSON(t, fmt.Sprintf("%s/api/jobs/%d", schedulerURL, nightlyHarnessCLIUpdateJobID), &job)

	if job.ID != nightlyHarnessCLIUpdateJobID {
		t.Fatalf("scheduler answered job %d for id %d", job.ID, nightlyHarnessCLIUpdateJobID)
	}
	if !job.Enabled {
		t.Errorf("job %d (%s) is switched off, so the harness CLIs are not kept up to date", job.ID, job.Name)
	}
	if job.Type != "shell" {
		t.Errorf("job %d type = %q, want shell", job.ID, job.Type)
	}
	if !strings.Contains(job.Command, harnessCLIUpdateScript) {
		t.Errorf("job %d command = %q, want it to run %s", job.ID, job.Command, harnessCLIUpdateScript)
	}
	if !runsEveryDay(job.Schedule) {
		t.Errorf("job %d schedule = %q, want one that fires every day (day of month, month and day of week all *)", job.ID, job.Schedule)
	}

	// The script the job names must be in this repo and executable, or the
	// job fails every night.
	script := filepath.Join("..", "..", harnessCLIUpdateScript)
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("%s: %v", harnessCLIUpdateScript, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %s)", harnessCLIUpdateScript, info.Mode())
	}

	var runs []schedulerRun
	getSchedulerJSON(t, fmt.Sprintf("%s/api/jobs/%d/runs", schedulerURL, nightlyHarnessCLIUpdateJobID), &runs)
	if len(runs) == 0 {
		t.Logf("job %d has not run yet", job.ID)
		return
	}
	latest := runs[0]
	for _, run := range runs[1:] {
		if run.StartedAt.After(latest.StartedAt) {
			latest = run
		}
	}
	if latest.Status != "success" {
		t.Errorf("job %d's last run (%d, %s) ended %q:\n%s", job.ID, latest.ID, latest.StartedAt.Format(time.RFC3339), latest.Status, truncateString(latest.Output, 2000))
	} else {
		t.Logf("job %d last ran %s: %s", job.ID, latest.StartedAt.Format(time.RFC3339), strings.TrimSpace(lastLines(latest.Output, 4)))
	}
	if age := time.Since(latest.StartedAt); age > 49*time.Hour {
		t.Errorf("job %d last ran %s ago; a nightly job should have run within two days", job.ID, age.Round(time.Hour))
	}
}

// productionSchedulerURL reads the scheduler's port from its own unit, the
// way deploy.sh does when it writes LLMBRIDGE_SCHEDULER_URL.
func productionSchedulerURL(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home directory: %v", err)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", "scheduler.service")
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the scheduler's unit: %v", err)
	}
	match := regexp.MustCompile(`(?m)^Environment=SCHEDULER_PORT=(\d+)\s*$`).FindSubmatch(unit)
	if match == nil {
		t.Fatalf("no Environment=SCHEDULER_PORT= line in %s", unitPath)
	}
	return "http://localhost:" + string(match[1])
}

func getSchedulerJSON(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("decode %s: %v: %s", url, err, truncateString(string(body), 400))
	}
}

// runsEveryDay reports whether a 5-field cron schedule fires every day.
func runsEveryDay(schedule string) bool {
	fields := strings.Fields(schedule)
	return len(fields) == 5 && fields[2] == "*" && fields[3] == "*" && fields[4] == "*"
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
