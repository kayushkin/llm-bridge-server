package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Disk usage on the Machines page: what fills a runner machine's drives, and
// deleting what its owner picks (llm-bridge/msg/disk_usage.go says how the
// runner does it). These routes pass requests to the machine's runner and
// its answers back, and keep the record of every deletion in table
// machine_disk_deletions: who asked, for what, and how it ended.
//
// A deletion is guarded here too: the caller must send the entry's name as
// confirmation_name, exactly, as the page makes the owner type it, and the
// row is written before the runner is asked, so nothing is deleted without
// a record. All routes are operator routes.

// diskUsageRequestTimeout bounds a wait for a runner's answer. A helper
// command answers in a second or two; a slow one is still under this.
const diskUsageRequestTimeout = 45 * time.Second

// machineDiskDeletionsShown is how many recorded deletions a status shows.
const machineDiskDeletionsShown = 20

// MachineDiskUsageView answers GET /machines/{id}/disk-usage.
type MachineDiskUsageView struct {
	MachineID string `json:"machine_id"`
	// Available says whether the runner is connected with a disk-usage
	// helper; UnavailableReason says why not.
	Available         bool                        `json:"available"`
	UnavailableReason string                      `json:"unavailable_reason,omitempty"`
	Helper            *msg.DiskUsageTool          `json:"helper,omitempty"`
	Status            *msg.DiskUsageStatus        `json:"status,omitempty"`
	Deletions         []store.MachineDiskDeletion `json:"deletions"`
}

// StartMachineDiskUsage wires runners' finished deletions and disconnects
// to the deletion record.
func (s *Server) StartMachineDiskUsage() {
	runners := s.harness.Runners()
	runners.SetDiskUsageDeletionReceiver(s.recordFinishedDiskDeletion)
	runners.SetDisconnectReceiver(func(machineID string) {
		if s.runnerConnectionOfMachineID(machineID) != nil {
			return // a newer connection replaced this one; its deletion carries on
		}
		lost, err := s.store.MarkUnfinishedMachineDiskDeletionsLost(machineID)
		if err != nil {
			log.Printf("[disk-usage] ERROR: mark deletions on %s lost: %v", machineID, err)
		} else if lost > 0 {
			log.Printf("[disk-usage] the runner on %s went away with %d deletions unfinished; marked lost", machineID, lost)
		}
	})
}

func (s *Server) recordFinishedDiskDeletion(machineID string, deletion msg.DiskUsageDeletion) {
	if err := s.store.UpdateMachineDiskDeletion(machineID, deletion.DeletionID, deletion, ""); err != nil {
		log.Printf("[disk-usage] ERROR: record the end of deletion %s on %s: %v", deletion.DeletionID, machineID, err)
		return
	}
	log.Printf("[disk-usage] deletion %s of %s on %s %s: %d bytes in %d files deleted, %d entries failed",
		deletion.DeletionID, deletion.Path, machineID, deletion.State, deletion.DeletedBytes, deletion.DeletedFileCount, deletion.FailedEntryCount)
}

func (s *Server) runnerConnectionOfMachineID(machineID string) *harness.RunnerConnection {
	for _, connection := range s.harness.Runners().List() {
		if connection.MachineID == machineID {
			return connection
		}
	}
	return nil
}

// diskUsageRunner finds the machine and its connected runner with a helper,
// or writes the error and returns nil.
func (s *Server) diskUsageRunner(w http.ResponseWriter, r *http.Request) (*msg.Machine, *harness.RunnerConnection, string) {
	machine, err := s.harnessStore.GetMachine(r.PathValue("id"))
	if err != nil || machine == nil {
		writeJSONError(w, http.StatusNotFound, "machine_not_found", "no machine "+r.PathValue("id"))
		return nil, nil, ""
	}
	if machine.Transport != msg.TransportRunner {
		return machine, nil, fmt.Sprintf("%s is not a runner machine; disk usage is shown only for machines with a runner", machine.Name)
	}
	connection := s.harness.Runners().Get(machine.Name)
	if connection == nil || connection.MachineID != machine.ID {
		return machine, nil, fmt.Sprintf("the runner on %s is not connected", machine.Name)
	}
	if connection.Hello == nil || connection.Hello.DiskUsage == nil {
		return machine, nil, fmt.Sprintf("the runner on %s has no disk-usage helper: only a runner in WSL on Windows has one so far, and a runner built before 2026-11-24 has none", machine.Name)
	}
	return machine, connection, ""
}

func (s *Server) askDiskUsage(r *http.Request, connection *harness.RunnerConnection, request msg.DiskUsageRequest) (*msg.DiskUsageAnswer, error) {
	ctx, cancel := context.WithTimeout(r.Context(), diskUsageRequestTimeout)
	defer cancel()
	return connection.RequestDiskUsage(ctx, request)
}

// writeDiskUsageAnswerError answers a failed request: 502 when the runner
// could not be asked, 409 when it answered that it could not.
func writeDiskUsageAnswerError(w http.ResponseWriter, answer *msg.DiskUsageAnswer, err error) bool {
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "runner_did_not_answer", err.Error())
		return true
	}
	if answer.Error != "" {
		writeJSONError(w, http.StatusConflict, "runner_refused", answer.Error)
		return true
	}
	return false
}

// handleMachineDiskUsage serves GET /machines/{id}/disk-usage.
func (s *Server) handleMachineDiskUsage(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	view := MachineDiskUsageView{MachineID: machine.ID, UnavailableReason: unavailable}
	deletions, err := s.store.MachineDiskDeletions(machine.ID, machineDiskDeletionsShown)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "disk_deletions_unreadable", err.Error())
		return
	}
	view.Deletions = deletions
	if connection != nil {
		view.Helper = connection.Hello.DiskUsage
		answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{Action: msg.DiskUsageActionStatus})
		switch {
		case err != nil:
			view.UnavailableReason = err.Error()
		case answer.Error != "":
			view.UnavailableReason = answer.Error
		default:
			view.Available = true
			view.Status = answer.Status
		}
	}
	writeJSON(w, view)
}

// handleStartMachineDiskScan serves POST /machines/{id}/disk-usage/scans {"drive":"C:"}.
func (s *Server) handleStartMachineDiskScan(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	if connection == nil {
		writeJSONError(w, http.StatusConflict, "disk_usage_unavailable", unavailable)
		return
	}
	var body struct {
		Drive string `json:"drive"`
	}
	if err := decodeStrictJSON(r, &body); err != nil || body.Drive == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", `send {"drive":"C:"}`)
		return
	}
	answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{Action: msg.DiskUsageActionStartScan, Drive: body.Drive})
	if writeDiskUsageAnswerError(w, answer, err) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(answer.Status)
}

// handleMachineDiskEntries serves GET /machines/{id}/disk-usage/entries?path=C:\Games:
// an entry of the drive's latest scan and the entries directly inside it.
func (s *Server) handleMachineDiskEntries(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	if connection == nil {
		writeJSONError(w, http.StatusConflict, "disk_usage_unavailable", unavailable)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSONError(w, http.StatusBadRequest, "path_required", `send ?path=C:\ for a drive's root`)
		return
	}
	answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{Action: msg.DiskUsageActionListChildren, Path: path})
	if writeDiskUsageAnswerError(w, answer, err) {
		return
	}
	writeJSON(w, answer.Children)
}

// handleCheckMachineDiskDeletion serves POST /machines/{id}/disk-usage/deletion-checks {"path"}:
// whether the entry may be deleted, why not, and what to know first. It deletes nothing.
func (s *Server) handleCheckMachineDiskDeletion(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	if connection == nil {
		writeJSONError(w, http.StatusConflict, "disk_usage_unavailable", unavailable)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeStrictJSON(r, &body); err != nil || body.Path == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", `send {"path":"D:\\Games\\Old"}`)
		return
	}
	answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{Action: msg.DiskUsageActionCheckDeletion, Path: body.Path})
	if writeDiskUsageAnswerError(w, answer, err) {
		return
	}
	writeJSON(w, answer.DeletionCheck)
}

// MachineDiskDeletionRequest is the body of POST /machines/{id}/disk-usage/deletions.
type MachineDiskDeletionRequest struct {
	Path string                 `json:"path"`
	Kind msg.DiskUsageEntryKind `json:"kind"`
	// ConfirmationName is the entry's name as the owner typed it. It must be
	// exactly the last part of Path.
	ConfirmationName  string `json:"confirmation_name"`
	ExpectedSizeBytes uint64 `json:"expected_size_bytes"`
}

// MachineDiskDeletionAnswer answers POST /machines/{id}/disk-usage/deletions.
type MachineDiskDeletionAnswer struct {
	Deletion      store.MachineDiskDeletion   `json:"deletion"`
	DeletionCheck *msg.DiskUsageDeletionCheck `json:"deletion_check,omitempty"`
}

// handleDeleteFromMachineDisk serves POST /machines/{id}/disk-usage/deletions:
// 202 once the runner has started deleting, 409 with the reason when it
// refused, 400 when the confirmation does not match.
func (s *Server) handleDeleteFromMachineDisk(w http.ResponseWriter, r *http.Request) {
	machine, connection, unavailable := s.diskUsageRunner(w, r)
	if machine == nil {
		return
	}
	if connection == nil {
		writeJSONError(w, http.StatusConflict, "disk_usage_unavailable", unavailable)
		return
	}
	var body MachineDiskDeletionRequest
	if err := decodeStrictJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.Kind != msg.DiskUsageEntryFile && body.Kind != msg.DiskUsageEntryDirectory {
		writeJSONError(w, http.StatusBadRequest, "invalid_kind", `kind must be "file" or "directory"`)
		return
	}
	name := diskPathName(body.Path)
	if name == "" || body.ConfirmationName != name {
		writeJSONError(w, http.StatusBadRequest, "confirmation_mismatch", fmt.Sprintf("confirmation_name must be exactly %q, the name of what is deleted", name))
		return
	}
	requestedBy, _ := principalIdentityOfRequest(r)
	deletion, err := s.store.RecordRequestedMachineDiskDeletion(store.MachineDiskDeletion{
		MachineID:              machine.ID,
		Path:                   body.Path,
		Kind:                   body.Kind,
		ExpectedSizeBytes:      body.ExpectedSizeBytes,
		RequestedByPrincipalID: requestedBy,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "disk_deletion_not_recorded", err.Error())
		return
	}
	log.Printf("[disk-usage] %s asked to delete %s (%s, %d bytes) on %s as %s", principalLabel(requestedBy), body.Path, body.Kind, body.ExpectedSizeBytes, machine.Name, deletion.ID)

	answer, err := s.askDiskUsage(r, connection, msg.DiskUsageRequest{
		Action: msg.DiskUsageActionDelete, Path: body.Path, ExpectedKind: body.Kind, DeletionID: deletion.ID,
	})
	settle := func(state msg.DiskUsageDeletionState, reason string) {
		now := time.Now().UTC()
		deletion.State, deletion.RefusalReason, deletion.FinishedAt = state, reason, &now
		if err := s.store.UpdateMachineDiskDeletion(machine.ID, deletion.ID, msg.DiskUsageDeletion{DeletionID: deletion.ID, State: state, FinishedAt: &now}, reason); err != nil {
			log.Printf("[disk-usage] ERROR: record deletion %s as %s: %v", deletion.ID, state, err)
		}
	}
	switch {
	case err != nil:
		// The runner may have started; only its report can say.
		if markErr := s.store.UpdateMachineDiskDeletion(machine.ID, deletion.ID, msg.DiskUsageDeletion{DeletionID: deletion.ID, State: msg.DiskUsageDeletionLost}, ""); markErr != nil {
			log.Printf("[disk-usage] ERROR: record deletion %s as lost: %v", deletion.ID, markErr)
		}
		writeJSONError(w, http.StatusBadGateway, "runner_did_not_answer", err.Error()+"; scan again to see whether anything was deleted")
		return
	case answer.Error != "":
		settle(msg.DiskUsageDeletionRefused, answer.Error)
		writeJSONError(w, http.StatusConflict, "runner_refused", answer.Error)
		return
	case answer.DeletionCheck != nil && !answer.DeletionCheck.Allowed:
		settle(msg.DiskUsageDeletionRefused, answer.DeletionCheck.RefusalReason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(MachineDiskDeletionAnswer{Deletion: deletion, DeletionCheck: answer.DeletionCheck})
		return
	case answer.Status == nil || answer.Status.Deletion == nil || answer.Status.Deletion.DeletionID != deletion.ID:
		settle(msg.DiskUsageDeletionFailed, "the runner answered without starting this deletion")
		writeJSONError(w, http.StatusBadGateway, "runner_answer_unexpected", "the runner answered without starting this deletion")
		return
	}
	// The runner may already have reported the end; record running only over requested.
	if err := s.store.MarkMachineDiskDeletionRunning(machine.ID, deletion.ID); err != nil {
		log.Printf("[disk-usage] ERROR: record deletion %s as running: %v", deletion.ID, err)
	}
	deletion.State = msg.DiskUsageDeletionRunning
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(MachineDiskDeletionAnswer{Deletion: deletion, DeletionCheck: answer.DeletionCheck})
}

// handleMachineDiskDeletions serves GET /machines/{id}/disk-usage/deletions:
// the recorded deletions on a machine, newest first.
func (s *Server) handleMachineDiskDeletions(w http.ResponseWriter, r *http.Request) {
	machine, err := s.harnessStore.GetMachine(r.PathValue("id"))
	if err != nil || machine == nil {
		writeJSONError(w, http.StatusNotFound, "machine_not_found", "no machine "+r.PathValue("id"))
		return
	}
	deletions, err := s.store.MachineDiskDeletions(machine.ID, 500)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "disk_deletions_unreadable", err.Error())
		return
	}
	writeJSON(w, deletions)
}

func principalLabel(principalID string) string {
	if principalID == "" {
		return "the internal service"
	}
	return principalID
}

// decodeStrictJSON decodes a request body, refusing a field the type lacks.
func decodeStrictJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("one JSON object only")
	}
	return nil
}

// diskPathName is the last part of a path on a runner machine: a Windows
// path splits on backslashes, a WSL path, which starts with /, on slashes.
func diskPathName(path string) string {
	separator := `\`
	if strings.HasPrefix(path, "/") {
		separator = "/"
	}
	return path[strings.LastIndex(path, separator)+1:]
}
