package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Request types are canonical — defined in llm-bridge/msg/server.go.
// DO NOT define new request/response types here. Add them to msg/ instead,
// then run generate-ts.sh so the TypeScript frontend stays in sync.
type (
	CreateInstanceRequest = msg.CreateInstanceRequest
	BindCredentialRequest = msg.BindCredentialRequest
)

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.harnessStore.ListInstances()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if instances == nil {
		instances = []msg.Instance{}
	}
	writeJSON(w, instances)
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.harnessStore.GetInstance(id)
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

	transport := msg.TransportLocal
	if req.Transport == "ssh" {
		transport = msg.TransportSSH
	}
	host := req.Host
	if host == "" {
		host = "localhost"
	}
	maxSessions := req.MaxConcurrentSessions
	if maxSessions == 0 {
		maxSessions = 1
	}

	inst := &msg.Instance{
		ID:                    fmt.Sprintf("inst_%d", time.Now().UnixNano()),
		HarnessType:           msg.Harness(req.HarnessType),
		Name:                  req.Name,
		Host:                  host,
		Transport:             transport,
		SSHUser:               req.SSHUser,
		SSHKeyPath:            req.SSHKeyPath,
		SSHPort:               req.SSHPort,
		WorkingDir:            req.WorkingDir,
		MaxConcurrentSessions: maxSessions,
		Enabled:               true,
	}

	if err := s.harnessStore.CreateInstance(inst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

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

	// Merge fields
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.HarnessType != "" {
		existing.HarnessType = msg.Harness(req.HarnessType)
	}
	if req.Host != "" {
		existing.Host = req.Host
	}
	if req.Transport != "" {
		if req.Transport == "ssh" {
			existing.Transport = msg.TransportSSH
		} else {
			existing.Transport = msg.TransportLocal
		}
	}
	if req.SSHUser != "" {
		existing.SSHUser = req.SSHUser
	}
	if req.SSHKeyPath != "" {
		existing.SSHKeyPath = req.SSHKeyPath
	}
	if req.SSHPort != 0 {
		existing.SSHPort = req.SSHPort
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
	inst, err := s.harnessStore.GetInstance(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	// Get credential bindings from harness-store
	credBindings, _ := s.harnessStore.ListInstanceCredentials(id)
	if credBindings == nil {
		credBindings = []msg.InstanceCredential{}
	}

	// SSH reachability check for remote instances
	reachable := true
	if inst.Transport == msg.TransportSSH {
		reachable = s.harness.CheckSSHReachability(inst)
	}

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

	// Get all sessions and filter by instance
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
