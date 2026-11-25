package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// WSL compaction on the Machines page (llm-bridge msg.DiskUsageWSLCompaction):
// setting it up shows the machine's owner one UAC prompt at the machine, and
// a run shuts the machine's WSL down for minutes, stopping its runner, every
// session on it and anything else running in WSL. So a run must be confirmed
// as such, and is refused while a session on the machine is mid-turn.

// handleSetUpMachineWSLCompaction serves POST /machines/{id}/disk-usage/wsl-compaction/set-up.
func (s *Server) handleSetUpMachineWSLCompaction(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	if connection == nil {
		writeJSONError(w, http.StatusConflict, "disk_usage_unavailable", unavailable)
		return
	}
	requestedBy, _ := principalIdentityOfRequest(r)
	log.Printf("[disk-usage] %s asked to set up WSL compaction on %s; the runner shows a UAC prompt there", principalLabel(requestedBy), machine.Name)
	answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{Action: msg.DiskUsageActionSetUpWSLCompaction})
	if writeDiskUsageAnswerError(w, answer, err) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(answer.Status)
}

// MachineWSLCompactionRequest is the body of POST /machines/{id}/disk-usage/wsl-compaction/runs.
type MachineWSLCompactionRequest struct {
	// ConfirmShutdown must be true: the caller has said WSL may be shut
	// down, with the runner and every session on the machine.
	ConfirmShutdown bool `json:"confirm_shutdown"`
}

// handleStartMachineWSLCompaction serves POST /machines/{id}/disk-usage/wsl-compaction/runs.
func (s *Server) handleStartMachineWSLCompaction(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	if connection == nil {
		writeJSONError(w, http.StatusConflict, "disk_usage_unavailable", unavailable)
		return
	}
	var body MachineWSLCompactionRequest
	if err := decodeStrictJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if !body.ConfirmShutdown {
		writeJSONError(w, http.StatusBadRequest, "shutdown_not_confirmed", `compacting shuts WSL down, with the runner and every session on this machine; send {"confirm_shutdown":true}`)
		return
	}
	busy, err := s.sessionsMidTurnOnMachine(machine.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "session_store_unreadable", err.Error())
		return
	}
	if len(busy) > 0 {
		writeJSONError(w, http.StatusConflict, "sessions_running_on_machine",
			fmt.Sprintf("sessions on %s are working now and compacting would stop them: %s; wait for them to finish or stop them first", machine.Name, strings.Join(busy, ", ")))
		return
	}
	requestedBy, _ := principalIdentityOfRequest(r)
	log.Printf("[disk-usage] %s asked to compact WSL on %s; WSL shuts down there in about ten seconds", principalLabel(requestedBy), machine.Name)
	answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{Action: msg.DiskUsageActionCompactWSL})
	if writeDiskUsageAnswerError(w, answer, err) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(answer.Status)
}

// sessionsMidTurnOnMachine names the sessions on an instance of machineID
// whose turn or background work is running, as "name (id)".
func (s *Server) sessionsMidTurnOnMachine(machineID string) ([]string, error) {
	var busy []string
	machineOfInstance := map[string]string{}
	for _, state := range sessionStatesARestartCutsOff() {
		sessions, err := s.store.ListSessionsByState(string(state))
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			if session.InstanceID == "" || !s.harness.HasProcess(session.SessionID) {
				continue
			}
			instanceMachine, known := machineOfInstance[session.InstanceID]
			if !known {
				instance, err := s.harnessStore.GetInstance(session.InstanceID)
				if err != nil || instance == nil {
					return nil, fmt.Errorf("read instance %s of session %s: %v", session.InstanceID, session.SessionID, err)
				}
				instanceMachine = instance.MachineID
				machineOfInstance[session.InstanceID] = instanceMachine
			}
			if instanceMachine == machineID {
				busy = append(busy, fmt.Sprintf("%s (%s)", session.DisplayName, session.SessionID))
			}
		}
	}
	return busy, nil
}
