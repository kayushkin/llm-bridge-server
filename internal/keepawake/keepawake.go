// Package keepawake holds the machine this server runs on awake while its
// sessions run, and for a grace period after, by keeping a child process that
// the operating system honours as a sleep block.
//
// A block only stops the machine from going to sleep on its own. It cannot wake
// a machine that is already asleep, and it does not stop a closed lid or the
// power button from putting it to sleep.
package keepawake

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Inhibitor starts a sleep block. Hold returns once the block is in force.
type Inhibitor interface {
	Hold(reason string) (Hold, error)
	// Describe names how the block is held, for the log.
	Describe() string
}

// Hold is one sleep block in force. Release ends it and waits for it to end.
type Hold interface {
	Release() error
}

// Settings are the keep_awake settings as they are now. The controller asks
// for them at every check, so a change takes effect without a restart.
type Settings struct {
	Enabled bool
	Grace   time.Duration
}

// Controller decides when to hold a block and holds at most one.
type Controller struct {
	inhibitor Inhibitor
	settings  func() Settings
	// sessionsRunning says whether any session on this server is running now.
	sessionsRunning func() (bool, error)
	now             func() time.Time

	mutex        sync.Mutex
	hold         Hold
	lastActivity time.Time
	lastError    string
}

// NewController builds a controller. now is time.Now outside tests.
func NewController(inhibitor Inhibitor, settings func() Settings, sessionsRunning func() (bool, error), now func() time.Time) *Controller {
	return &Controller{inhibitor: inhibitor, settings: settings, sessionsRunning: sessionsRunning, now: now}
}

// Touch records activity — a message sent to a session, or a session started —
// and checks at once, so a block starts before the work does.
func (controller *Controller) Touch() {
	controller.mutex.Lock()
	controller.lastActivity = controller.now()
	controller.mutex.Unlock()
	controller.Check()
}

// Check takes or releases the block for what is true now: held while the
// setting is on and a session runs, or until the grace period after the last
// activity has passed.
func (controller *Controller) Check() {
	settings := controller.settings()
	running := false
	var runningError error
	if settings.Enabled {
		running, runningError = controller.sessionsRunning()
	}
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	now := controller.now()
	if runningError != nil {
		// Not knowing is not the same as idle: keep whatever is held, and say why.
		controller.lastError = "could not tell whether a session is running: " + runningError.Error()
		log.Printf("[keep-awake] ERROR: %s", controller.lastError)
		return
	}
	if running {
		controller.lastActivity = now
	}
	wanted := settings.Enabled && (running || (!controller.lastActivity.IsZero() && now.Before(controller.lastActivity.Add(settings.Grace))))
	switch {
	case wanted && controller.hold == nil:
		hold, err := controller.inhibitor.Hold("llm-bridge-server sessions are running")
		if err != nil {
			controller.lastError = err.Error()
			log.Printf("[keep-awake] ERROR: hold the machine awake with %s: %v", controller.inhibitor.Describe(), err)
			return
		}
		controller.hold = hold
		controller.lastError = ""
		log.Printf("[keep-awake] holding the machine awake with %s", controller.inhibitor.Describe())
	case !wanted && controller.hold != nil:
		controller.releaseLocked()
	}
}

// releaseLocked ends the block. The caller holds the mutex.
func (controller *Controller) releaseLocked() {
	if err := controller.hold.Release(); err != nil {
		controller.lastError = "release: " + err.Error()
		log.Printf("[keep-awake] ERROR: release the block held with %s: %v", controller.inhibitor.Describe(), err)
	} else {
		log.Printf("[keep-awake] released: the machine may sleep again")
	}
	controller.hold = nil
}

// State is what GET /server reports.
func (controller *Controller) State() msg.ServerKeepAwake {
	settings := controller.settings()
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	state := msg.ServerKeepAwake{Enabled: settings.Enabled, Held: controller.hold != nil, Error: controller.lastError}
	if state.Held && !controller.lastActivity.IsZero() {
		until := controller.lastActivity.Add(settings.Grace).UTC()
		state.Until = &until
	}
	return state
}

// Run checks every interval until ctx ends, then releases any block.
func (controller *Controller) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	controller.Check()
	for {
		select {
		case <-ctx.Done():
			controller.Close()
			return
		case <-ticker.C:
			controller.Check()
		}
	}
}

// Close releases any block. It is safe to call more than once.
func (controller *Controller) Close() {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.hold != nil {
		controller.releaseLocked()
	}
}
