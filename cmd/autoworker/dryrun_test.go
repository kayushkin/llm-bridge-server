package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// -dry-run promises a tick changes nothing: it publishes no session, writes no
// cooldown, and creates no kanban board. TestADryRunIsNotAShortfall pins the
// first. These pin the other two, at the call sites in run() that choose them;
// initQueueClient's own read-only behaviour is pinned in
// queue_client_bootstrap_test.go, but that cannot see run() passing it the
// wrong argument.
//
// Each is paired with the real run that must still do the thing. Without the
// pair, a binary that never saved state or never bootstrapped would satisfy
// every "did not happen" assertion.
//
// Ported from 3fec1ffd, whose run(runConfig) extraction main already has in
// its own shape: run(cfg, modelRoleFor, publish).

func TestADryRunWritesNoCooldown(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())
	cfg.dryRun = true

	captureLog(t, func() { run(cfg, defaultLaneModelRole, (&recordingPublisher{}).publish) })

	if _, err := os.Stat(cfg.statePath); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote the cooldown state (stat err=%v); the next real tick "+
			"would read a fire that never happened and stay quiet", err)
	}
}

func TestARealDispatchWritesTheCooldown(t *testing.T) {
	usage := newStubUsage(t, firingLimits())
	cfg := tickConfig(t, usage.URL(), newStubKanban(t).URL(), newStubSpend(t).URL())

	captureLog(t, func() { run(cfg, defaultLaneModelRole, (&recordingPublisher{}).publish) })

	st, err := loadState(cfg.statePath)
	if err != nil {
		t.Fatalf("loadState after a real dispatch: %v", err)
	}
	if st.LastFire["anthropic"] == 0 {
		t.Errorf("a real dispatch left no last fire for anthropic (%v); the next tick "+
			"would dispatch again inside the cooldown", st.LastFire)
	}
}

// quietLimits opens no gate: anthropic is already at 100% of its week.
func quietLimits() map[string]any {
	limits := firingLimits()
	limits["anthropic"].(map[string]any)["windows"].(map[string]any)["seven_day"].(map[string]any)["used_percent"] = 100.0
	return limits
}

// writeCountingBoards is a kanban-store with no boards that refuses every write
// and counts it. The peek at the end of a quiet tick is the only thing that
// reaches it, so the count is the number of times that peek tried to bootstrap.
func writeCountingBoards(t *testing.T) (url string, writes func() int) {
	t.Helper()
	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/boards" {
			writeStubJSON(w, 200, []map[string]string{})
			return
		}
		if r.Method != "GET" {
			mu.Lock()
			count++
			mu.Unlock()
		}
		http.Error(w, "refused by the test", 500)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int { mu.Lock(); defer mu.Unlock(); return count }
}

func TestADryRunCreatesNoQueueBoard(t *testing.T) {
	kanban, writes := writeCountingBoards(t)
	cfg := tickConfig(t, newStubUsage(t, quietLimits()).URL(), kanban, newStubSpend(t).URL())
	cfg.dryRun = true

	captureLog(t, func() { run(cfg, defaultLaneModelRole, (&recordingPublisher{}).publish) })

	if n := writes(); n != 0 {
		t.Errorf("a dry run sent %d writes to kanban-store; on a fresh install it would "+
			"create the queue board", n)
	}
}

func TestAQuietRealTickBootstrapsTheQueueBoard(t *testing.T) {
	kanban, writes := writeCountingBoards(t)
	cfg := tickConfig(t, newStubUsage(t, quietLimits()).URL(), kanban, newStubSpend(t).URL())

	captureLog(t, func() { run(cfg, defaultLaneModelRole, (&recordingPublisher{}).publish) })

	if writes() == 0 {
		t.Error("a real tick against a store with no queue board never tried to create it; " +
			"the dry-run test above would then pass for the wrong reason")
	}
}
