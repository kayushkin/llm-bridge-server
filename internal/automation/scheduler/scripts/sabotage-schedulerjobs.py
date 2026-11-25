#!/usr/bin/env python3
"""Score internal/schedulerjobs' tests by breaking the package on purpose.

The scoring engine lives in scripts/sabotage.py. This file is only the case list:
one edit per mechanism the suite is meant to pin.

    python3 scripts/sabotage-schedulerjobs.py [--diffs|--crosstable]

Seven binaries register their cron job through this package. Before 2026-09-26
each carried its own copy of the client, and the rows below were spread across
the autoworker, kanban-classifier, kanban-curator and dispatcher-degradation
scorers, each aimed at that binary's copy. Those scorers keep only the rows for
their own literals — the job name, cadence and command — and the mechanism rows
live here, scored against this package's own tests and no one else's.

Ported from refactor/one-scheduler-job-registration-client (2339268e) and its
scorer commits, re-aimed at main's version of the package, which takes the
scheduler URL from its caller.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from sabotage import REPO, Case, score  # noqa: E402

TARGETS = [REPO / "internal/schedulerjobs/schedulerjobs.go"]
PACKAGES = ["./internal/schedulerjobs/"]


CASES = [
    # ---- controls ----
    Case(
        "CONTROL known-positive: nothing is ever registered",
        [("\treturn create(schedulerURL, job)\n}", "\treturn nil\n}")],
    ),
    Case(
        "CONTROL known-negative: reword the already-registered message",
        [('fmt.Printf("%s already registered; no changes made\\n", job.Name)',
          'fmt.Printf("%s is registered already\\n", job.Name)')],
        expected_unnoticed="stdout nothing asserts on; the harness must not call this a catch",
    ),

    # ---- the payload ----
    Case(
        "the registered job is a type the scheduler runs as a prompt, not a command",
        [('"type":        "shell",', '"type":        "agent",')],
    ),
    Case(
        "the job name sent is not the one the duplicate check looks for",
        [('"name":        job.Name,', '"name":        job.Name + "-v2",')],
    ),
    Case(
        "the job runs on a cadence nobody chose",
        [('"schedule":    job.Schedule,', '"schedule":    "* * * * *",')],
    ),
    Case(
        "the job runs something other than the command it was given",
        [('"command":     job.Command,', '"command":     "true",')],
    ),
    Case(
        "the description is lost",
        [('"description": job.Description,', '"description": "",')],
    ),
    Case(
        "a job's timeout is never sent, so the scheduler's 5-minute default kills long ticks",
        [("if job.TimeoutSeconds != 0 {", "if false {")],
    ),
    Case(
        "a zero timeout is sent anyway",
        [("if job.TimeoutSeconds != 0 {", "if true {")],
    ),

    # ---- the duplicate check ----
    Case(
        "an existing job is not recognized, so every -register adds another copy",
        [("if name == job.Name {", "if name == job.Name && false {")],
    ),
    Case(
        "the name check matches by prefix, so a neighboring job proves this one is registered",
        [("if name == job.Name {", "if strings.HasPrefix(name, job.Name) {")],
    ),
    Case(
        "a job list the scheduler could not serve is read as no jobs",
        [("if response.StatusCode != http.StatusOK {", "if false {")],
    ),
    Case(
        "any 2xx job list is accepted, though only 200 carries the list",
        [("if response.StatusCode != http.StatusOK {", "if response.StatusCode >= 300 {")],
    ),
    Case(
        "a job list that is not a JSON array is read as no jobs",
        [("if err := json.Unmarshal(body, &jobs); err != nil {",
          "if err := json.Unmarshal(body, &jobs); false && err != nil {")],
    ),
    Case(
        "a job list cut off in transit is read as whatever arrived",
        [('\t\treturn nil, fmt.Errorf("list jobs: read body: %w", err)', "\t\t_ = err")],
    ),
    Case(
        "an unreachable scheduler reads as an empty job list",
        [('\t\treturn nil, fmt.Errorf("list jobs: %w", err)', "\t\treturn nil, nil")],
    ),
    Case(
        "the job list is read at a path the scheduler does not serve",
        [('http.Get(schedulerURL + "/api/jobs")', 'http.Get(schedulerURL + "/api/job")')],
    ),

    # ---- the create ----
    Case(
        "the job is posted to a path the scheduler does not serve",
        [('http.NewRequest("POST", schedulerURL+"/api/jobs"', 'http.NewRequest("POST", schedulerURL+"/api/job"')],
    ),
    Case(
        "the job body goes out without a JSON content type",
        [('request.Header.Set("Content-Type", "application/json")', 'request.Header.Set("Content-Type", "text/plain")')],
    ),
    Case(
        "a scheduler that refuses the job is reported as a successful registration",
        [("if response.StatusCode >= 300 {", "if response.StatusCode >= 300 && false {")],
    ),
    Case(
        "300 stops being a refusal, so the one status on the boundary reads as success",
        [("if response.StatusCode >= 300 {", "if response.StatusCode >= 301 {")],
    ),
    Case(
        "299 becomes a refusal, so a success on the boundary reads as a failure",
        [("if response.StatusCode >= 300 {", "if response.StatusCode >= 299 {")],
    ),
]


if __name__ == "__main__":
    sys.exit(score(TARGETS, PACKAGES, CASES))
