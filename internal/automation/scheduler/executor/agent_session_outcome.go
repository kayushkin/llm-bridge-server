package executor

import (
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/runstatus"
)

// DefaultAgentSessionWaitLimit is how long an agent run waits for its session
// when the job sets no timeout_seconds. Job sessions measured from 2026-09-12
// to 2026-10-20 finished their turns within twelve minutes; an hour leaves room
// for a slow one without letting a stuck session hold the job for a day.
const DefaultAgentSessionWaitLimit = time.Hour

// defaultSessionPollInterval is how often a run reads its session's state.
const defaultSessionPollInterval = 10 * time.Second

// AgentSessionWaitLimit is the longest a run of a job with this timeout_seconds
// waits for its session to settle.
func AgentSessionWaitLimit(timeoutSeconds int) time.Duration {
	if timeoutSeconds > 0 {
		return time.Duration(timeoutSeconds) * time.Second
	}
	return DefaultAgentSessionWaitLimit
}

// A run used to end as success the moment its message reached NATS. On
// 2026-09-14 the Claude subscription had hit its weekly limit: every job
// session that day got one error reply and spent nothing, and the scheduler
// recorded all four runs as success. So a run now follows its session to the
// end of the turn it started, and its status is what the session did.

// runResultFromSession reads one snapshot of the session a run published to.
// It returns settled=false while the session has not yet settled since
// publishedAt.
//
// A state stamped before publishedAt belongs to an earlier turn and says
// nothing about this one. The scheduler creates a session and sends its first
// message in one inbound message, so a new session goes from starting straight
// into its turn and is never idle before it; any settled state stamped after
// publishedAt is therefore the end of the turn this run started.
//
//   - error, aborted, disconnected: the turn failed or the session was taken
//     away, and the run is an error.
//   - idle, completed, awaiting_user: the turn ended, and the run succeeded.
//   - anything else: still working, or parked on a permission prompt; wait.
func runResultFromSession(session *msg.ManagedSession, publishedAt time.Time) (result Result, settled bool) {
	if session.Status == nil || session.Status.ChangedAt.Before(publishedAt) {
		return Result{}, false
	}
	status := session.Status
	description := describeSessionStatus(session)
	switch status.State {
	case msg.SessionError, msg.SessionAborted, msg.SessionDisconnected:
		return Result{Status: runstatus.Error, Output: "session " + session.SessionID + " ended in " + description}, true
	case msg.SessionIdle, msg.SessionCompleted, msg.SessionAwaitingUser:
		return Result{Status: runstatus.Success, Output: "session " + session.SessionID + " finished its turn: " + description}, true
	}
	return Result{}, false
}

// describeSessionStatus says a session's state, why it got there, and what it
// spent, for a run's output.
func describeSessionStatus(session *msg.ManagedSession) string {
	status := session.Status
	description := string(status.State)
	if status.Reason != "" {
		description += " (" + string(status.Reason) + ")"
	}
	if status.ReasonDetail != "" {
		description += ": " + status.ReasonDetail
	}
	return description + fmt.Sprintf(", spent $%.2f", session.SpendUSD)
}

// readSession reads one session from llm-bridge-server.
func (e *AgentExecutor) readSession(sessionID string) (*msg.ManagedSession, error) {
	if e.bridge == nil {
		return nil, fmt.Errorf("the agent executor has no llm-bridge-server to read session %s from", sessionID)
	}
	return e.bridge.Session(sessionID)
}

// waitForSessionOutcome polls the session until runResultFromSession settles
// it, or until waitLimit passes. A session the bridge does not know yet, or a
// read that fails, is waited out: the adapter creates the session a moment
// after the publish. When the limit
// passes the run is a timeout, and the session is left as it is.
func (e *AgentExecutor) waitForSessionOutcome(sessionID string, publishedAt time.Time, waitLimit time.Duration) Result {
	pollInterval := e.sessionPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultSessionPollInterval
	}
	deadline := publishedAt.Add(waitLimit)
	lastSeen := "the bridge never answered"
	for {
		session, err := e.readSession(sessionID)
		switch {
		case err == nil:
			if result, settled := runResultFromSession(session, publishedAt); settled {
				return result
			}
			if session.Status == nil {
				lastSeen = "session " + sessionID + " has no status"
			} else {
				lastSeen = "session " + sessionID + " was " + describeSessionStatus(session)
			}
		case errors.Is(err, ErrSessionNotFound):
			lastSeen = "session " + sessionID + " never appeared on the bridge"
		default:
			lastSeen = fmt.Sprintf("reading session %s failed: %v", sessionID, err)
		}
		if !time.Now().Add(pollInterval).Before(deadline) {
			return Result{Status: runstatus.Timeout, Output: fmt.Sprintf("no outcome after %s; %s (left running)", waitLimit, lastSeen)}
		}
		time.Sleep(pollInterval)
	}
}
