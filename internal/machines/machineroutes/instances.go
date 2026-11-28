package machineroutes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge/msg"
)

// CreateInstanceRequest is re-exported from msg so package consumers
// don't need a separate import.
type (
	CreateInstanceRequest = msg.CreateInstanceRequest
	BindCredentialRequest = msg.BindCredentialRequest
)

// ListInstancesWithMachines populates each instance's Machine field by
// joining against the machines table. Used by every list/get response so
// the client gets host/transport/SSH details for display.
func (s *Service) ListInstancesWithMachines() ([]msg.Instance, error) {
	insts, err := s.HarnessStore().ListInstances()
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return []msg.Instance{}, nil
	}
	machines, err := s.HarnessStore().ListMachines()
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

func (s *Service) HandleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.ListInstancesWithMachines()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, restricted := requestcaller.PrincipalRestrictingRequest(r); restricted {
		caller, _ := requestcaller.FromRequest(r)
		instances, err = s.instancesPrincipalMayDispatchOn(r.Context(), grantcheck.SubjectOfCaller(caller), instances)
		if err != nil {
			httpjson.WriteJSONError(w, http.StatusBadGateway, "grant_store_unavailable", err.Error())
			return
		}
	}
	httpjson.WriteJSON(w, instances)
}

// instancesPrincipalMayDispatchOn narrows instances to the ones a principal
// restricted to itself may start a session on, by the rule
// checkPrincipalMayRunHere enforces at creation (principal_grants.go). Machine
// details (hostname, SSH user and key path) are the operator's and are left
// out; machine_id stays.
func (s *Service) instancesPrincipalMayDispatchOn(ctx context.Context, subject grantcheck.Subject, instances []msg.Instance) ([]msg.Instance, error) {
	dispatch, err := s.GrantChecker.DispatchGrantsOf(ctx, subject)
	if err != nil {
		return nil, err
	}
	visible := make([]msg.Instance, 0, len(instances))
	for _, instance := range instances {
		if !dispatch.Permits(instance.ID, instance.MachineID) {
			continue
		}
		instance.Machine = nil
		visible = append(visible, instance)
	}
	return visible, nil
}

func (s *Service) HandleGetInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.HarnessStore().GetInstanceWithMachine(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	httpjson.WriteJSON(w, inst)
}

func (s *Service) HandleCreateInstance(w http.ResponseWriter, r *http.Request) {
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
	machine, err := s.HarnessStore().GetMachine(req.MachineID)
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
	if err := s.HarnessStore().CreateInstance(inst); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inst.Machine = machine

	w.WriteHeader(http.StatusCreated)
	httpjson.WriteJSON(w, inst)
}

func (s *Service) HandleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.HarnessStore().GetInstance(id)
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
		if _, err := s.HarnessStore().GetMachine(req.MachineID); err != nil {
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

	if err := s.HarnessStore().UpdateInstance(existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if m, err := s.HarnessStore().GetMachine(existing.MachineID); err == nil {
		existing.Machine = m
	}
	httpjson.WriteJSON(w, existing)
}

func (s *Service) HandleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.HarnessStore().DeleteInstance(id); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) HandleListInstanceCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.HarnessStore().GetInstance(id); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	creds, err := s.HarnessStore().ListInstanceCredentials(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if creds == nil {
		creds = []msg.InstanceCredential{}
	}
	httpjson.WriteJSON(w, creds)
}

func (s *Service) HandleBindCredential(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if _, err := s.HarnessStore().GetInstance(instanceID); err != nil {
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
	if err := s.HarnessStore().BindCredential(ic); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	httpjson.WriteJSON(w, ic)
}

func (s *Service) HandleUnbindCredential(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	credID := r.PathValue("cred_id")

	if err := s.HarnessStore().UnbindCredential(instanceID, credID); err != nil {
		http.Error(w, "binding not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) HandleInstanceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.HarnessStore().GetInstanceWithMachine(id)
	if err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	credBindings, _ := s.HarnessStore().ListInstanceCredentials(id)
	if credBindings == nil {
		credBindings = []msg.InstanceCredential{}
	}

	reachable, checkedAt := s.Harness.ReachabilityOfMachine(inst.Machine)

	status := msg.InstanceStatus{
		Instance:    *inst,
		Credentials: credBindings,
		Reachable:   reachable,
		LastChecked: checkedAt,
	}
	httpjson.WriteJSON(w, status)
}

func (s *Service) HandleListInstanceSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.HarnessStore().GetInstance(id); err != nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	sessions, err := s.Store.ListSessions()
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
	httpjson.WriteJSON(w, instanceSessions)
}
