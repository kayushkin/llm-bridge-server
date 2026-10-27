package server

// What this server calls itself, and whether it holds its machine awake.
//
// server.name is a stored behaviour setting: GET /server answers it to any
// signed-in caller, every request and answer over a peer connection carries it
// in msg.ServerNameHeader, and a change is sent at once to every connected
// server (peers.go, tellConnectedServersThisServersName). keep_awake.* drive
// internal/keepawake, which holds a sleep block while sessions run here.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/keepawake"
	"github.com/kayushkin/llm-bridge/msg"
)

// longestServerName bounds server.name: it travels in a header and is drawn
// as a folder heading.
const longestServerName = 80

// keepAwakeCheckInterval is how often keep-awake looks at the sessions.
const keepAwakeCheckInterval = 30 * time.Second

// thisServerName is server.name as it is now, trimmed. Empty means none.
func (s *Server) thisServerName() string {
	return strings.TrimSpace(s.settings.String(config.SettingServerName))
}

// checkServerNameValue refuses a name that cannot travel in a header or be
// drawn on one line.
func checkServerNameValue(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed != value {
		return errors.New("a server name has no spaces at either end")
	}
	if len([]rune(value)) > longestServerName {
		return fmt.Errorf("a server name is at most %d characters", longestServerName)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return errors.New("a server name has no control characters or line breaks")
		}
	}
	return nil
}

// checkKeepAwakeGraceMinutesValue refuses a negative grace.
func checkKeepAwakeGraceMinutesValue(value string) error {
	if strings.HasPrefix(strings.TrimSpace(value), "-") {
		return errors.New("keep_awake.grace_minutes is 0 or more")
	}
	return nil
}

// handleGetServer: GET /server.
func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, msg.ServerDescription{Name: s.thisServerName(), KeepAwake: s.keepAwakeState()})
}

func (s *Server) keepAwakeSettings() keepawake.Settings {
	return keepawake.Settings{
		Enabled: s.settings.Boolean(config.SettingKeepAwakeEnabled),
		Grace:   time.Duration(s.settings.Integer(config.SettingKeepAwakeGraceMinutes)) * time.Minute,
	}
}

func (s *Server) keepAwakeState() msg.ServerKeepAwake {
	if s.keepAwake == nil {
		return msg.ServerKeepAwake{Enabled: s.keepAwakeSettings().Enabled}
	}
	return s.keepAwake.State()
}

// sessionsRunningHere says whether any session here is mid-turn.
func (s *Server) sessionsRunningHere() (bool, error) {
	states := make([]string, len(harness.HoldingSessionStates))
	for index, state := range harness.HoldingSessionStates {
		states[index] = string(state)
	}
	count, err := s.store.CountSessionsInStates(states)
	return count > 0, err
}

// unavailableInhibitor stands in on a system keep-awake has no way to hold:
// every hold fails with why, which GET /server shows.
type unavailableInhibitor struct{ reason error }

func (inhibitor unavailableInhibitor) Hold(string) (keepawake.Hold, error) {
	return nil, inhibitor.reason
}
func (inhibitor unavailableInhibitor) Describe() string { return "nothing" }

// StartKeepAwake starts keep-awake's checks and returns the function that
// stops them and releases any block. Called once from main.
func (s *Server) StartKeepAwake() (stop func()) {
	inhibitor, err := keepawake.SystemInhibitor()
	if err != nil {
		inhibitor = unavailableInhibitor{reason: err}
	}
	s.keepAwake = keepawake.NewController(inhibitor, s.keepAwakeSettings, s.sessionsRunningHere, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.keepAwake.Run(ctx, keepAwakeCheckInterval)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// touchingKeepAwake wraps a handler that sends work to a session, so a block
// starts before the work does.
func (s *Server) touchingKeepAwake(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.keepAwake != nil {
			// Taking a block can wait on a child starting; the send must not.
			go s.keepAwake.Touch()
		}
		handler(w, r)
	}
}
