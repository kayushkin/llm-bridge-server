package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// sessionStatesARestartCutsOff are the states in which restarting this server
// would cut work off: a turn in progress (harness.HoldingSessionStates), or a
// finished turn whose background work still runs inside the harness process.
func sessionStatesARestartCutsOff() []msg.SessionState {
	states := append([]msg.SessionState{}, harness.HoldingSessionStates...)
	return append(states, msg.SessionBackgroundTasksRunning)
}

// sessionARestartWouldCutOff is one session that is doing work right now.
type sessionARestartWouldCutOff struct {
	SessionID       string               `json:"session_id"`
	DisplayName     string               `json:"display_name"`
	State           string               `json:"state"`
	BackgroundTasks []msg.BackgroundTask `json:"background_tasks,omitempty"`
}

// restartReadiness answers whether this server can restart without cutting
// off work, and if not, which sessions it would cut off.
type restartReadiness struct {
	Ready        bool                         `json:"ready"`
	BusySessions []sessionARestartWouldCutOff `json:"busy_sessions"`
}

// handleRestartReadiness serves GET /restart-readiness, which deploy.sh polls
// before it stops the service, so a deploy waits for running turns and
// background agents to finish instead of killing them. A session counts only
// when its row is in a busy state and its harness process is running here: a
// row left busy by a process that is gone would otherwise hold every deploy
// until deploy.sh's time limit. A harness subagent has no process of its own,
// so it never counts; its parent does, as background_tasks_running.
func (s *Server) handleRestartReadiness(w http.ResponseWriter, r *http.Request) {
	answer := restartReadiness{BusySessions: []sessionARestartWouldCutOff{}}
	for _, state := range sessionStatesARestartCutsOff() {
		sessions, err := s.store.ListSessionsByState(string(state))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "session_store_unreadable", err.Error())
			return
		}
		for _, session := range sessions {
			if !s.harness.HasProcess(session.SessionID) {
				continue
			}
			busy := sessionARestartWouldCutOff{SessionID: session.SessionID, DisplayName: session.DisplayName, State: session.State}
			if state == msg.SessionBackgroundTasksRunning {
				tasks, err := s.store.LastReportedBackgroundTasks(session.SessionID)
				if err != nil {
					writeJSONError(w, http.StatusInternalServerError, "session_store_unreadable", err.Error())
					return
				}
				busy.BackgroundTasks = tasks
			}
			answer.BusySessions = append(answer.BusySessions, busy)
		}
	}
	answer.Ready = len(answer.BusySessions) == 0
	writeJSON(w, answer)
}
