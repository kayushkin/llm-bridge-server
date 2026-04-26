package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge/msg"
)

// handleEnrollRunner consumes a one-time enrollment passphrase, creates
// (or updates) the machine row for the runner, mints a durable per-machine
// runner token, and seeds a default claude_code instance pointing at it.
//
// Idempotency: re-enrollment with the same machine_name reuses the existing
// machine row (refreshing its hostname/os/etc. from the runner's report)
// and rotates the token. Different machine_name produces a new row.
func (s *Server) handleEnrollRunner(w http.ResponseWriter, r *http.Request) {
	if s.harnessStore == nil {
		http.Error(w, "harness store unavailable", http.StatusServiceUnavailable)
		return
	}

	var req msg.EnrollRunnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Passphrase == "" {
		http.Error(w, "passphrase is required", http.StatusBadRequest)
		return
	}
	if req.Hostname == "" {
		http.Error(w, "hostname is required", http.StatusBadRequest)
		return
	}

	machineName := req.MachineName
	if machineName == "" {
		machineName = req.Hostname
	}

	// Find or create the machine.
	machine, err := s.harnessStore.GetMachineByName(machineName)
	created := false
	if err != nil {
		machine = &msg.Machine{
			ID:                "m_" + randomHex(8),
			Name:              machineName,
			Hostname:          req.Hostname,
			OS:                req.OS,
			Arch:              req.Arch,
			Transport:         msg.TransportRunner,
			User:              req.User,
			DefaultWorkingDir: req.WorkingDir,
		}
		if err := s.harnessStore.CreateMachine(machine); err != nil {
			http.Error(w, fmt.Sprintf("create machine: %v", err), http.StatusInternalServerError)
			return
		}
		created = true
	} else {
		// Refresh fields the runner just reported so the row reflects
		// reality even after re-enrollment from a moved/upgraded host.
		if machine.Transport == "" {
			machine.Transport = msg.TransportRunner
		}
		machine.Hostname = req.Hostname
		machine.OS = req.OS
		machine.Arch = req.Arch
		machine.User = req.User
		if req.WorkingDir != "" {
			machine.DefaultWorkingDir = req.WorkingDir
		}
		if err := s.harnessStore.UpdateMachine(machine); err != nil {
			http.Error(w, fmt.Sprintf("update machine: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Consume the passphrase, binding it to this machine. Done after
	// machine create so a failed consume doesn't leave a half-enrolled
	// row — but we roll back our created machine if the consume fails.
	if err := s.harnessStore.ConsumeEnrollment(req.Passphrase, machine.ID); err != nil {
		if created {
			_ = s.harnessStore.DeleteMachine(machine.ID)
		}
		switch {
		case errors.Is(err, harnessstore.ErrEnrollmentExpired):
			http.Error(w, err.Error(), http.StatusGone)
		default:
			http.Error(w, fmt.Sprintf("consume enrollment: %v", err), http.StatusUnauthorized)
		}
		return
	}

	// Mint and store the runner token.
	token, err := harnessstore.GenerateRunnerToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("mint token: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.harnessStore.SetMachineRunnerTokenHash(machine.ID, harnessstore.HashRunnerToken(token)); err != nil {
		http.Error(w, fmt.Sprintf("store token: %v", err), http.StatusInternalServerError)
		return
	}
	_ = s.harnessStore.TouchMachineLastSeen(machine.ID)

	// Seed default instances based on what the runner reports as
	// available. For each available harness type that doesn't already
	// have an instance bound to this machine, create one.
	defaultHarnesses := []msg.Harness{msg.HarnessClaudeCode}
	if len(req.AvailableHarnesses) > 0 {
		defaultHarnesses = nil
		for _, ah := range req.AvailableHarnesses {
			if ah.Harness == msg.HarnessClaudeCode {
				defaultHarnesses = append(defaultHarnesses, ah.Harness)
			}
		}
		// If the runner reports no claude_code, fall back to first
		// available harness so the machine isn't useless.
		if len(defaultHarnesses) == 0 && len(req.AvailableHarnesses) > 0 {
			defaultHarnesses = []msg.Harness{req.AvailableHarnesses[0].Harness}
		}
	}

	existing, _ := s.harnessStore.ListInstancesByMachine(machine.ID)
	hasHarness := make(map[msg.Harness]bool, len(existing))
	for _, e := range existing {
		hasHarness[e.HarnessType] = true
	}
	var instanceIDs []string
	for _, h := range defaultHarnesses {
		if hasHarness[h] {
			continue
		}
		inst := &msg.Instance{
			ID:                    fmt.Sprintf("inst_%d", time.Now().UnixNano()),
			HarnessType:           h,
			Name:                  fmt.Sprintf("%s/%s", machine.Name, h),
			MachineID:             machine.ID,
			MaxConcurrentSessions: 1,
			Enabled:               true,
		}
		if err := s.harnessStore.CreateInstance(inst); err != nil {
			http.Error(w, fmt.Sprintf("create default instance %s: %v", h, err), http.StatusInternalServerError)
			return
		}
		instanceIDs = append(instanceIDs, inst.ID)
	}

	resp := msg.EnrollRunnerResponse{
		MachineID:   machine.ID,
		MachineName: machine.Name,
		RunnerToken: token,
		InstanceIDs: instanceIDs,
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, resp)
}
