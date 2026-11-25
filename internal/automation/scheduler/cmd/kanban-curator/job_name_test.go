package main

import "testing"

// The names here are the ones scheduler jobs 13 and 18 carried on 2026-09-11,
// when -board (a name) became -board-id. A re-register must derive the same
// name from the board's stored name or it registers a second job beside the
// first. Job 17, kanban-curator-tcl-tick, was named by hand with -job-name and
// is not derivable; that flag stays for it.
func TestTheDefaultJobNameIsTheOneTheLiveJobsCarry(t *testing.T) {
	for _, check := range []struct{ boardName, want string }{
		{"Agent runs", "kanban-curator-tick"},
		{"Autoworker queue", "kanban-curator-autoworker-queue-tick"},
		{"Task completion loop", "kanban-curator-task-completion-loop-tick"},
	} {
		if got := defaultJobName(check.boardName); got != check.want {
			t.Errorf("defaultJobName(%q) = %q, want %q", check.boardName, got, check.want)
		}
	}
}
