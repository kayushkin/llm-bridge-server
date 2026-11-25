package executor

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
)

func sessionIn(state msg.SessionState, reason msg.SessionStateReason, changedAt time.Time) *msg.ManagedSession {
	return &msg.ManagedSession{
		SessionID: "scheduler-claude-code-1",
		State:     string(state),
		Status:    &msg.SessionStatus{State: state, Reason: reason, ChangedAt: changedAt},
	}
}

// The 2026-09-14 case: the session failed at once and spent nothing, and the
// run must say so instead of reporting success.
func TestASessionThatEndsInErrorFailsTheRun(t *testing.T) {
	publishedAt := time.Now()
	for _, state := range []msg.SessionState{msg.SessionError, msg.SessionAborted, msg.SessionDisconnected} {
		result, settled := runResultFromSession(sessionIn(state, msg.ReasonHarnessError, publishedAt.Add(2*time.Second)), publishedAt)
		if !settled || result.Status != runstatus.Error {
			t.Errorf("%s: got settled=%v status=%q, want a settled error", state, settled, result.Status)
		}
		if !strings.Contains(result.Output, string(state)) || !strings.Contains(result.Output, "scheduler-claude-code-1") {
			t.Errorf("%s: output %q does not name the session and its state", state, result.Output)
		}
	}
}

func TestASessionWhoseTurnEndedSucceeds(t *testing.T) {
	publishedAt := time.Now()
	for _, state := range []msg.SessionState{msg.SessionIdle, msg.SessionCompleted, msg.SessionAwaitingUser} {
		result, settled := runResultFromSession(sessionIn(state, msg.ReasonTurnComplete, publishedAt.Add(time.Minute)), publishedAt)
		if !settled || result.Status != runstatus.Success {
			t.Errorf("%s: got settled=%v status=%q, want a settled success", state, settled, result.Status)
		}
	}
}

// A job that continues its session finds it settled from the previous run. That
// state is stamped before the publish and says nothing about this turn.
func TestAStateFromBeforeThePublishIsNotThisTurnsOutcome(t *testing.T) {
	publishedAt := time.Now()
	for _, state := range []msg.SessionState{msg.SessionIdle, msg.SessionError, msg.SessionCompleted} {
		if _, settled := runResultFromSession(sessionIn(state, msg.ReasonTurnComplete, publishedAt.Add(-time.Hour)), publishedAt); settled {
			t.Errorf("%s stamped before the publish settled the run", state)
		}
	}
}

func TestAWorkingOrParkedSessionIsWaitedFor(t *testing.T) {
	publishedAt := time.Now()
	for _, state := range []msg.SessionState{msg.SessionStarting, msg.SessionModelGenerating, msg.SessionToolRunning,
		msg.SessionCompacting, msg.SessionBackgroundTasksRunning, msg.SessionRateLimited, msg.SessionAwaitingPermission, msg.SessionPaused} {
		if _, settled := runResultFromSession(sessionIn(state, msg.ReasonTurnStarted, publishedAt.Add(time.Second)), publishedAt); settled {
			t.Errorf("%s settled the run while the turn was not over", state)
		}
	}
	if _, settled := runResultFromSession(&msg.ManagedSession{SessionID: "s", State: "idle"}, publishedAt); settled {
		t.Error("a session with no status settled the run")
	}
}

// sessionAnswers is llm-bridge-server answering a run's reads of its session
// with each of answers in turn, repeating the last one.
type sessionAnswers struct {
	t       *testing.T
	answers []func() (*msg.ManagedSession, error)
	calls   int32
}

func (bridge *sessionAnswers) Instances() ([]msg.Instance, error) {
	bridge.t.Error("an outcome wait read the instances")
	return nil, nil
}

func (bridge *sessionAnswers) Session(sessionID string) (*msg.ManagedSession, error) {
	if sessionID != "scheduler-claude-code-1" {
		bridge.t.Errorf("unexpected session %s", sessionID)
	}
	call := int(atomic.AddInt32(&bridge.calls, 1)) - 1
	if call >= len(bridge.answers) {
		call = len(bridge.answers) - 1
	}
	return bridge.answers[call]()
}

func fakeBridge(t *testing.T, answers ...func() (*msg.ManagedSession, error)) (*AgentExecutor, *int32) {
	t.Helper()
	bridge := &sessionAnswers{t: t, answers: answers}
	return &AgentExecutor{bridge: bridge, sessionPollInterval: time.Millisecond}, &bridge.calls
}

func answerSession(session *msg.ManagedSession) func() (*msg.ManagedSession, error) {
	return func() (*msg.ManagedSession, error) { return session, nil }
}

func answerNoSuchSession() (*msg.ManagedSession, error) {
	return nil, fmt.Errorf("session scheduler-claude-code-1: %w", ErrSessionNotFound)
}

// The adapter creates the session a moment after the publish, so a 404 and a
// session still at work are waited out until the turn ends.
func TestTheRunFollowsTheSessionToTheEndOfItsTurn(t *testing.T) {
	publishedAt := time.Now()
	executor, _ := fakeBridge(t,
		answerNoSuchSession,
		answerSession(sessionIn(msg.SessionModelGenerating, msg.ReasonTurnStarted, publishedAt.Add(time.Millisecond))),
		answerSession(sessionIn(msg.SessionError, msg.ReasonHarnessError, publishedAt.Add(2*time.Millisecond))),
	)
	result := executor.waitForSessionOutcome("scheduler-claude-code-1", publishedAt, time.Minute)
	if result.Status != runstatus.Error || !strings.Contains(result.Output, "ended in error") {
		t.Fatalf("got %q %q, want the session's error", result.Status, result.Output)
	}
}

// A session that never finishes, or never appears, ends the run as a timeout
// that says what was last seen.
func TestASessionThatNeverSettlesTimesOut(t *testing.T) {
	publishedAt := time.Now()
	executor, _ := fakeBridge(t, answerSession(sessionIn(msg.SessionToolRunning, msg.ReasonToolCalled, publishedAt.Add(time.Millisecond))))
	result := executor.waitForSessionOutcome("scheduler-claude-code-1", publishedAt, 20*time.Millisecond)
	if result.Status != runstatus.Timeout || !strings.Contains(result.Output, "tool_running") {
		t.Fatalf("got %q %q, want a timeout naming tool_running", result.Status, result.Output)
	}

	executor, _ = fakeBridge(t, answerNoSuchSession)
	result = executor.waitForSessionOutcome("scheduler-claude-code-1", time.Now(), 20*time.Millisecond)
	if result.Status != runstatus.Timeout || !strings.Contains(result.Output, "never appeared") {
		t.Fatalf("got %q %q, want a timeout saying the session never appeared", result.Status, result.Output)
	}
}

func TestTheWaitLimitIsTheJobsTimeout(t *testing.T) {
	if got := AgentSessionWaitLimit(1800); got != 30*time.Minute {
		t.Errorf("timeout_seconds 1800 gave %s", got)
	}
	if got := AgentSessionWaitLimit(0); got != DefaultAgentSessionWaitLimit {
		t.Errorf("no timeout_seconds gave %s, want the default", got)
	}
}
