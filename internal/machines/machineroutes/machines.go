package machineroutes

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge/msg"
)

func (s *Service) HandleListMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := s.HarnessStore().ListMachines()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if machines == nil {
		machines = []msg.Machine{}
	}
	httpjson.WriteJSON(w, machines)
}

func (s *Service) HandleGetMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.HarnessStore().GetMachine(id)
	if err != nil {
		http.Error(w, "machine not found", http.StatusNotFound)
		return
	}
	httpjson.WriteJSON(w, m)
}

func (s *Service) HandleCreateMachine(w http.ResponseWriter, r *http.Request) {
	var req msg.CreateMachineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Transport == "" {
		req.Transport = msg.TransportLocal
	}
	if existing, _ := s.HarnessStore().GetMachineByName(req.Name); existing != nil {
		http.Error(w, fmt.Sprintf("machine name already in use: %s", req.Name), http.StatusConflict)
		return
	}
	m := &msg.Machine{
		ID:                "m_" + randomHex(8),
		Name:              req.Name,
		Emoji:             req.Emoji,
		Hostname:          req.Hostname,
		OS:                req.OS,
		Arch:              req.Arch,
		Transport:         req.Transport,
		SSHUser:           req.SSHUser,
		SSHKeyPath:        req.SSHKeyPath,
		SSHPort:           req.SSHPort,
		DefaultWorkingDir: req.DefaultWorkingDir,
		Notes:             req.Notes,
	}
	if err := s.HarnessStore().CreateMachine(m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	httpjson.WriteJSON(w, m)
}

func (s *Service) HandleUpdateMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.HarnessStore().GetMachine(id)
	if err != nil {
		http.Error(w, "machine not found", http.StatusNotFound)
		return
	}
	var req msg.UpdateMachineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Emoji != "" {
		existing.Emoji = req.Emoji
	}
	if req.Hostname != "" {
		existing.Hostname = req.Hostname
	}
	if req.OS != "" {
		existing.OS = req.OS
	}
	if req.Arch != "" {
		existing.Arch = req.Arch
	}
	if req.Transport != "" {
		existing.Transport = req.Transport
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
	if req.DefaultWorkingDir != "" {
		existing.DefaultWorkingDir = req.DefaultWorkingDir
	}
	if req.Notes != "" {
		existing.Notes = req.Notes
	}
	if err := s.HarnessStore().UpdateMachine(existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpjson.WriteJSON(w, existing)
}

func (s *Service) HandleDeleteMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.HarnessStore().DeleteMachine(id); err != nil {
		http.Error(w, "machine not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// randomHex returns 2*n hex characters from crypto/rand. Used as the
// volatile suffix on auto-generated IDs (machines, instances, …).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
