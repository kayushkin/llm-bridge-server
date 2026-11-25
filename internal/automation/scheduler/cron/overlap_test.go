package cron

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/executor"
)

// A tick landing while the previous run of the same job is still going must be
// dropped. Stacking them is what let one slow job pile up dozens of concurrent
// runs until the scheduler was OOM-killed.
func TestOverlappingTickIsSkipped(t *testing.T) {
	store := testStore(t)
	job, err := store.CreateJob("slow", "* * * * *", "irrelevant")
	if err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var concurrent, peak, starts int32

	s := NewWithExecutor(store, func(string, time.Duration) executor.Result {
		atomic.AddInt32(&starts, 1)
		n := atomic.AddInt32(&concurrent, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&concurrent, -1)
		return executor.Result{Status: "success"}
	})

	// First tick occupies the job; four more land while it is still running.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.executeJob(*job) }()

	waitFor(t, func() bool { return atomic.LoadInt32(&concurrent) == 1 })

	for i := 0; i < 4; i++ {
		if run := s.executeJob(*job); run != nil {
			t.Fatalf("overlapping tick %d executed instead of being skipped", i)
		}
	}

	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("executor ran %d times, want 1 (the 4 overlapping ticks must be dropped)", got)
	}
	if got := atomic.LoadInt32(&peak); got != 1 {
		t.Fatalf("peak concurrency was %d, want 1", got)
	}

	// Exactly one run row, and the job is runnable again once it is released.
	runs, err := store.ListRuns(job.ID, db.MaxRunsListed)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(runs))
	}
}

// The guard must release, or a job would run exactly once per process lifetime.
func TestJobRunsAgainAfterPreviousRunFinishes(t *testing.T) {
	store := testStore(t)
	job, err := store.CreateJob("quick", "* * * * *", "irrelevant")
	if err != nil {
		t.Fatal(err)
	}

	var calls int32
	s := NewWithExecutor(store, func(string, time.Duration) executor.Result {
		atomic.AddInt32(&calls, 1)
		return executor.Result{Status: "success"}
	})

	for i := 0; i < 3; i++ {
		if run := s.executeJob(*job); run == nil {
			t.Fatalf("sequential tick %d was skipped; the in-flight guard did not release", i)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("executor ran %d times, want 3", got)
	}
}

// The per-job timeout must reach the executor, not a hardcoded 5 minutes.
func TestPerJobTimeoutIsPassedToExecutor(t *testing.T) {
	store := testStore(t)
	job, err := store.CreateJobFull(db.Job{
		Name: "long", Schedule: "* * * * *", Command: "irrelevant",
		Type: "shell", TimeoutSecs: 7200,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got time.Duration
	s := NewWithExecutor(store, func(_ string, timeout time.Duration) executor.Result {
		got = timeout
		return executor.Result{Status: "success"}
	})
	s.executeJob(*job)

	if want := 2 * time.Hour; got != want {
		t.Fatalf("executor received timeout %v, want %v", got, want)
	}
}

// A job that sets no timeout keeps the executor default.
func TestUnsetTimeoutFallsBackToDefault(t *testing.T) {
	store := testStore(t)
	job, err := store.CreateJob("default", "* * * * *", "irrelevant")
	if err != nil {
		t.Fatal(err)
	}

	var got time.Duration
	s := NewWithExecutor(store, func(_ string, timeout time.Duration) executor.Result {
		got = timeout
		return executor.Result{Status: "success"}
	})
	s.executeJob(*job)

	if got != executor.DefaultTimeout {
		t.Fatalf("executor received timeout %v, want the default %v", got, executor.DefaultTimeout)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
