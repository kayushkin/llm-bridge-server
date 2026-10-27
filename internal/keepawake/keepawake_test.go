package keepawake

import (
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type fakeInhibitor struct {
	mutex    sync.Mutex
	holds    int
	releases int
	failNext error
}

func (inhibitor *fakeInhibitor) Describe() string { return "a fake" }

func (inhibitor *fakeInhibitor) Hold(string) (Hold, error) {
	inhibitor.mutex.Lock()
	defer inhibitor.mutex.Unlock()
	if inhibitor.failNext != nil {
		err := inhibitor.failNext
		inhibitor.failNext = nil
		return nil, err
	}
	inhibitor.holds++
	return fakeHold{inhibitor: inhibitor}, nil
}

type fakeHold struct{ inhibitor *fakeInhibitor }

func (hold fakeHold) Release() error {
	hold.inhibitor.mutex.Lock()
	defer hold.inhibitor.mutex.Unlock()
	hold.inhibitor.releases++
	return nil
}

// world is the clock, the settings and whether sessions run, all set by a test.
type world struct {
	now     time.Time
	enabled bool
	grace   time.Duration
	running bool
	failing error
}

func newControllerIn(t *testing.T, w *world) (*Controller, *fakeInhibitor) {
	t.Helper()
	inhibitor := &fakeInhibitor{}
	controller := NewController(inhibitor,
		func() Settings { return Settings{Enabled: w.enabled, Grace: w.grace} },
		func() (bool, error) { return w.running, w.failing },
		func() time.Time { return w.now })
	return controller, inhibitor
}

func TestHeldWhileSessionsRunAndForTheGraceAfter(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: true, grace: 30 * time.Minute}
	controller, inhibitor := newControllerIn(t, w)

	controller.Check()
	if controller.State().Held || inhibitor.holds != 0 {
		t.Fatalf("nothing running and no activity yet: want no block, got %+v", controller.State())
	}

	w.running = true
	controller.Check()
	if !controller.State().Held || inhibitor.holds != 1 {
		t.Fatalf("a session runs: want one block, got %+v after %d holds", controller.State(), inhibitor.holds)
	}
	w.now = w.now.Add(10 * time.Minute)
	controller.Check()
	if inhibitor.holds != 1 {
		t.Fatalf("a block already held must not be taken again: %d holds", inhibitor.holds)
	}

	lastRunning := w.now
	w.running = false
	w.now = w.now.Add(29 * time.Minute)
	controller.Check()
	state := controller.State()
	if !state.Held || state.Until == nil || !state.Until.Equal(lastRunning.Add(30*time.Minute)) {
		t.Fatalf("inside the grace after the last running check: want held until %s, got %+v", lastRunning.Add(30*time.Minute), state)
	}

	w.now = w.now.Add(2 * time.Minute)
	controller.Check()
	if controller.State().Held || inhibitor.releases != 1 {
		t.Fatalf("past the grace: want released once, got %+v after %d releases", controller.State(), inhibitor.releases)
	}
}

func TestAMessageSentHoldsForTheGraceWithNothingRunning(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: true, grace: 5 * time.Minute}
	controller, inhibitor := newControllerIn(t, w)
	controller.Touch()
	if !controller.State().Held || inhibitor.holds != 1 {
		t.Fatalf("a message sent: want a block at once, got %+v", controller.State())
	}
	w.now = w.now.Add(6 * time.Minute)
	controller.Check()
	if controller.State().Held {
		t.Fatalf("five minutes of grace passed: want released, got %+v", controller.State())
	}
}

func TestDisabledHoldsNothingAndSwitchingOffReleases(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: false, grace: 30 * time.Minute, running: true}
	controller, inhibitor := newControllerIn(t, w)
	controller.Touch()
	if controller.State().Held || inhibitor.holds != 0 {
		t.Fatalf("switched off: want no block, got %+v", controller.State())
	}
	w.enabled = true
	controller.Check()
	if !controller.State().Held {
		t.Fatalf("switched on with a session running: want a block")
	}
	w.enabled = false
	controller.Check()
	if controller.State().Held || inhibitor.releases != 1 {
		t.Fatalf("switched off again: want released, got %+v", controller.State())
	}
}

func TestZeroGraceReleasesAsSoonAsNothingRuns(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: true, running: true}
	controller, _ := newControllerIn(t, w)
	controller.Check()
	w.running = false
	controller.Check()
	if controller.State().Held {
		t.Fatalf("grace 0 and nothing running: want released, got %+v", controller.State())
	}
}

func TestAFailedHoldIsReportedAndTriedAgain(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: true, grace: time.Minute, running: true}
	controller, inhibitor := newControllerIn(t, w)
	inhibitor.failNext = errors.New("no logind session")
	controller.Check()
	if state := controller.State(); state.Held || state.Error == "" {
		t.Fatalf("a hold that failed: want not held and the error shown, got %+v", state)
	}
	controller.Check()
	if state := controller.State(); !state.Held || state.Error != "" {
		t.Fatalf("the next check: want held and the error cleared, got %+v", state)
	}
}

func TestNotKnowingWhetherSessionsRunKeepsWhatIsHeld(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: true, running: true}
	controller, inhibitor := newControllerIn(t, w)
	controller.Check()
	w.running = false
	w.failing = errors.New("database locked")
	w.now = w.now.Add(time.Hour)
	controller.Check()
	if state := controller.State(); !state.Held || inhibitor.releases != 0 || state.Error == "" {
		t.Fatalf("an unreadable session table must neither release nor hide the error: got %+v after %d releases", state, inhibitor.releases)
	}
}

func TestCloseReleasesAndCanRepeat(t *testing.T) {
	w := &world{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), enabled: true, running: true}
	controller, inhibitor := newControllerIn(t, w)
	controller.Check()
	controller.Close()
	controller.Close()
	if controller.State().Held || inhibitor.releases != 1 {
		t.Fatalf("close: want released exactly once, got %+v after %d releases", controller.State(), inhibitor.releases)
	}
}

// The child-process path, with cat standing in for a block holder: it ends
// when its standard input closes, as systemd-inhibit's cat and the WSL
// PowerShell do.
func TestACommandHoldEndsItsChildOnRelease(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("no cat on this machine")
	}
	inhibitor := commandInhibitor{description: "cat", command: func(string) []string { return []string{"cat"} }}
	hold, err := inhibitor.Hold("test")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	child := hold.(*childHold)
	if err := hold.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if child.child.ProcessState == nil || !child.child.ProcessState.Exited() {
		t.Fatalf("release returned while the child still runs: %v", child.child.ProcessState)
	}
}

func TestACommandThatEndsAtOnceIsAFailedHold(t *testing.T) {
	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("no false on this machine")
	}
	inhibitor := commandInhibitor{description: "false", command: func(string) []string { return []string{"false"} }}
	if _, err := inhibitor.Hold("test"); err == nil {
		t.Fatal("a child that exits at once holds nothing, and Hold must say so")
	}
}
