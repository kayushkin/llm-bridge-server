package server

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// claudeCodeLongestWakeupDelay is the furthest ahead a ScheduleWakeup call can
// fire: Claude Code clamps delaySeconds to 3600 and then rounds the due time
// up to the next minute (measured on 2.1.283: a 60-second wakeup set at
// 15:55:00.5 came due at 15:57:00).
const claudeCodeLongestWakeupDelay = time.Hour + time.Minute

// RedeliverWakeupsLostInRestart finds the ScheduleWakeup timers that died when
// this server last stopped, and fires each one at its due time by sending its
// prompt to the session, which starts the harness again. Claude Code keeps
// the timer inside the claude process, and a restart kills every harness
// process, so without this a session that ended its turn waiting on a wakeup
// sits idle for good.
//
// Call it once at startup, after ReconcileAndResume. A wakeup counts as lost
// when its session was not deliberately stopped (aborted: the user or the
// reaper killed it), and no user_message has been logged since it came due,
// since a wakeup that fires logs its prompt as one. A wakeup that came due
// longer ago than autoResumeWindow is logged and left, by the same rule that
// leaves older sessions for the user to resume.
func (s *Server) RedeliverWakeupsLostInRestart() {
	now := time.Now()
	oldestDueToRedeliver := now.Add(-autoResumeWindow)
	// A session's row changes when its turn ends, which is after its last
	// ScheduleWakeup call, and that call came at most
	// claudeCodeLongestWakeupDelay before the wakeup came due.
	oldestCallToRead := oldestDueToRedeliver.Add(-claudeCodeLongestWakeupDelay)

	sessionIDs, err := s.store.ListSessionIDsUpdatedSince(oldestCallToRead)
	if err != nil {
		log.Printf("[wakeup-redelivery] ERROR listing recent sessions: %v", err)
		return
	}
	var armed int
	for _, sessionID := range sessionIDs {
		wakeup, err := s.lostWakeup(sessionID, oldestCallToRead)
		if err != nil {
			log.Printf("[wakeup-redelivery] ERROR %s: %v", sessionID, err)
			continue
		}
		if wakeup == nil {
			continue
		}
		if wakeup.DueAt.Before(oldestDueToRedeliver) {
			log.Printf("[wakeup-redelivery] %s: wakeup %s came due at %s, more than %s ago; leaving it", sessionID, wakeup.ToolID, wakeup.DueAt.Format(time.RFC3339), autoResumeWindow)
			continue
		}
		armed++
		log.Printf("[wakeup-redelivery] %s: wakeup %s lost in the restart; delivering it at %s", sessionID, wakeup.ToolID, wakeup.DueAt.Format(time.RFC3339))
		armedWakeup := *wakeup
		time.AfterFunc(time.Until(wakeup.DueAt), func() {
			s.deliverLostWakeup(armedWakeup, oldestCallToRead)
		})
	}
	if armed > 0 {
		log.Printf("[wakeup-redelivery] armed %d wakeups lost in the restart", armed)
	}
}

// lostWakeup returns the session's latest ScheduleWakeup when it is lost as
// RedeliverWakeupsLostInRestart defines it, and nil otherwise.
func (s *Server) lostWakeup(sessionID string, oldestCallToRead time.Time) (*store.ScheduledWakeup, error) {
	sess, err := s.store.GetSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	if msg.SessionState(sess.State) == msg.SessionAborted || sess.ControlledBy == msg.ControlledByHarness {
		return nil, nil
	}
	wakeup, err := s.store.LatestScheduledWakeup(sessionID, oldestCallToRead)
	if err != nil {
		return nil, fmt.Errorf("read latest wakeup: %w", err)
	}
	if wakeup == nil {
		return nil, nil
	}
	fired, err := s.store.UserMessageLoggedSince(sessionID, wakeup.DueAt)
	if err != nil {
		return nil, fmt.Errorf("read messages since the wakeup came due: %w", err)
	}
	if fired {
		return nil, nil
	}
	return wakeup, nil
}

// deliverLostWakeup sends an armed wakeup's prompt, unless the session has
// moved on since it was armed: the agent scheduled or cancelled another
// wakeup, a message already came, or someone stopped the session.
func (s *Server) deliverLostWakeup(armed store.ScheduledWakeup, oldestCallToRead time.Time) {
	current, err := s.lostWakeup(armed.SessionID, oldestCallToRead)
	if err != nil {
		log.Printf("[wakeup-redelivery] ERROR %s: %v", armed.SessionID, err)
		return
	}
	if current == nil || current.ToolID != armed.ToolID {
		log.Printf("[wakeup-redelivery] %s: wakeup %s no longer pending; not delivering it", armed.SessionID, armed.ToolID)
		return
	}
	message := fmt.Sprintf("[llm-bridge-server restarted while your ScheduleWakeup was pending, which cancelled it. This is its prompt, sent by the server at the time it was due.]\n\n%s", armed.Prompt)
	status, body := s.serveInternally(http.MethodPost, "/sessions/"+url.PathEscape(armed.SessionID)+"/send", SendMessageRequest{Message: message})
	if status != http.StatusOK {
		log.Printf("[wakeup-redelivery] ERROR %s: sending wakeup %s answered %d: %s", armed.SessionID, armed.ToolID, status, body)
		return
	}
	log.Printf("[wakeup-redelivery] %s: delivered wakeup %s", armed.SessionID, armed.ToolID)
}
