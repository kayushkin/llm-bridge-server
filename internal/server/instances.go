package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// CreateInstanceRequest is re-exported from msg so package consumers
// don't need a separate import.
type (
	CreateInstanceRequest = msg.CreateInstanceRequest
	BindCredentialRequest = msg.BindCredentialRequest
)

// listInstancesWithMachines populates each instance's Machine field by
// joining against the machines table. Used by every list/get response so
// the client gets host/transport/SSH details for display.
func (s *Server) listInstancesWithMachines() ([]msg.Instance, error) {
	insts, err := s.harnessStore.ListInstances()
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return []msg.Instance{}, nil
	}
	machines, err := s.harnessStore.ListMachines()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*msg.Machine, len(machines))
	for i := range machines {
		byID[machines[i].ID] = &machines[i]
	}
	for i := range insts {
		insts[i].Machine = byID[insts[i].MachineID]
	}
	return insts, nil
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.listInstancesWithMachines()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, instances)
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.harnessStore.GetInstanceWithMachine(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	writeJSON(w, inst)
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req CreateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.HarnessType == "" {
		http.Error(w, "harness_type is required", http.StatusBadRequest)
		return
	}
	if req.MachineID == "" {
		http.Error(w, "machine_id is required", http.StatusBadRequest)
		return
	}
	machine, err := s.harnessStore.GetMachine(req.MachineID)
	if err != nil {
		http.Error(w, fmt.Sprintf("machine_id not found: %s", req.MachineID), http.StatusBadRequest)
		return
	}
	maxSessions := req.MaxConcurrentSessions
	if maxSessions == 0 {
		maxSessions = 1
	}

	inst := &msg.Instance{
		ID:                    fmt.Sprintf("inst_%d", time.Now().UnixNano()),
		HarnessType:           msg.Harness(req.HarnessType),
		Name:                  req.Name,
		MachineID:             req.MachineID,
		WorkingDir:            req.WorkingDir,
		MaxConcurrentSessions: maxSessions,
		Enabled:               true,
	}
	if err := s.harnessStore.CreateInstance(inst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inst.Machine = machine

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, inst)
}

func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.harnessStore.GetInstance(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	var req CreateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.HarnessType != "" {
		existing.HarnessType = msg.Harness(req.HarnessType)
	}
	if req.MachineID != "" {
		if _, err := s.harnessStore.GetMachine(req.MachineID); err != nil {
			http.Error(w, fmt.Sprintf("machine_id not found: %s", req.MachineID), http.StatusBadRequest)
			return
		}
		existing.MachineID = req.MachineID
	}
	if req.WorkingDir != "" {
		existing.WorkingDir = req.WorkingDir
	}
	if req.MaxConcurrentSessions != 0 {
		existing.MaxConcurrentSessions = req.MaxConcurrentSessions
	}

	if err := s.harnessStore.UpdateInstance(existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if m, err := s.harnessStore.GetMachine(existing.MachineID); err == nil {
		existing.Machine = m
	}
	writeJSON(w, existing)
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.harnessStore.DeleteInstance(id); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListInstanceCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.harnessStore.GetInstance(id); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	creds, err := s.harnessStore.ListInstanceCredentials(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if creds == nil {
		creds = []msg.InstanceCredential{}
	}
	writeJSON(w, creds)
}

func (s *Server) handleBindCredential(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if _, err := s.harnessStore.GetInstance(instanceID); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	var req BindCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.CredentialID == "" {
		http.Error(w, "credential_id is required", http.StatusBadRequest)
		return
	}

	ic := &msg.InstanceCredential{
		InstanceID:   instanceID,
		CredentialID: req.CredentialID,
		Priority:     req.Priority,
		Enabled:      true,
	}
	if err := s.harnessStore.BindCredential(ic); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, ic)
}

func (s *Server) handleUnbindCredential(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	credID := r.PathValue("cred_id")

	if err := s.harnessStore.UnbindCredential(instanceID, credID); err != nil {
		http.Error(w, "binding not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInstanceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.harnessStore.GetInstanceWithMachine(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	credBindings, _ := s.harnessStore.ListInstanceCredentials(id)
	if credBindings == nil {
		credBindings = []msg.InstanceCredential{}
	}

	reachable := s.harness.CheckSSHReachability(inst.Machine)

	status := msg.InstanceStatus{
		Instance:    *inst,
		Credentials: credBindings,
		Reachable:   reachable,
		LastChecked: time.Now(),
	}
	writeJSON(w, status)
}

func (s *Server) handleListInstanceSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.harnessStore.GetInstance(id); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	sessions, err := s.store.ListSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var instanceSessions []interface{}
	for _, sess := range sessions {
		if sess.InstanceID == id {
			instanceSessions = append(instanceSessions, sess)
		}
	}
	if instanceSessions == nil {
		instanceSessions = []interface{}{}
	}
	writeJSON(w, instanceSessions)
}
