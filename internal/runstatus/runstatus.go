// Package runstatus holds the vocabulary of states a job run can be in.
//
// The vocabulary was spelled out as string literals in the places that happened
// to need one, and those places did not agree. On this branch the executor
// writes "success" and "error", the database writes "running", and the finish
// logger asked whether a run was "error" or "failed" — a status nothing in the
// repository has ever written.
//
// The dead literal matters because of what sits behind it. The deployed
// scheduler binary also ends runs as "timeout" (the run outlived its
// timeout_seconds cap and its process group was killed) and "interrupted" (the
// scheduler died while the run was in flight); both are documented service
// behaviour and both are on unmerged branches rather than here. Against a
// hand-listed pair of failure statuses, each of those lands at info level: a
// job killed by its own cap is filed indistinguishably from one that succeeded.
//
// So this package is the one place the vocabulary is written down, and the
// question "did this run fail?" is asked of it rather than of a literal list at
// the call site.
package runstatus

const (
	// Running is the state a run is inserted in and stays in until it finishes.
	Running = "running"

	// Success means the run finished on its own terms.
	Success = "success"

	// Error means the run finished and reported a failure.
	Error = "error"

	// Timeout means the run exceeded its timeout_seconds cap and its process
	// group was killed. Written by the deployed executor; not yet on this
	// branch.
	Timeout = "timeout"

	// Interrupted means the scheduler process died while the run was in flight,
	// and the run was closed out at the next startup. Written by startup
	// reconciliation rather than by the executor, so it never reaches the finish
	// logger. Written by the deployed scheduler; not yet on this branch.
	Interrupted = "interrupted"
)

// TerminalStatusIsFailure reports whether a finished run's status means the run
// did not complete on its own terms.
//
// Every status except Success counts, deliberately. A hand-listed set of
// failure statuses is what this function exists to replace: the finish logger
// listed two, one of which nothing produced, and neither of which was Timeout.
// A status this package has not heard of is far more likely to be a new way of
// failing than a new way of succeeding, so an unknown one is reported as a
// failure and shows up, rather than being filed at info and staying invisible.
// That is the property that makes the Timeout and Interrupted merges safe
// without anyone remembering to come back here.
//
// Running is not a terminal status and is outside this function's contract. It
// is reported as a failure on the same reasoning: a run being logged as finished
// while still marked running is itself a defect worth seeing.
func TerminalStatusIsFailure(status string) bool {
	return status != Success
}
