package machineroutes

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the machine, instance and runner routes on the server's
// mux. The instance and machine routes need harness-store and are left out
// without it; the runner routes and the harness proxy are always there.
func (s *Service) Mount(mux httproutes.Mux) {
	if s.HarnessStore() != nil {
		s.mountInstancesAndMachines(mux)
	}
	mux.HandleFunc("GET /api/runner/ws", s.HandleRunnerWS)
	mux.HandleFunc("POST /api/runner/enroll", s.HandleEnrollRunner)
	mux.HandleFunc("GET /api/runner/install.sh", s.HandleRunnerInstallScript)
	mux.HandleFunc("GET /api/runner/binary", s.HandleRunnerBinary)
	mux.HandleFunc("/api/harness-proxy/{harness}/{rest...}", s.HandleHarnessProxy)
}

func (s *Service) mountInstancesAndMachines(mux httproutes.Mux) {
	mux.HandleFunc("GET /instances", s.HandleListInstances)
	mux.HandleFunc("POST /instances", s.HandleCreateInstance)
	mux.HandleFunc("GET /instances/{id}", s.HandleGetInstance)
	mux.HandleFunc("PUT /instances/{id}", s.HandleUpdateInstance)
	mux.HandleFunc("DELETE /instances/{id}", s.HandleDeleteInstance)
	mux.HandleFunc("GET /instances/{id}/status", s.HandleInstanceStatus)
	mux.HandleFunc("GET /instances/{id}/sessions", s.HandleListInstanceSessions)
	mux.HandleFunc("GET /instances/{id}/credentials", s.HandleListInstanceCredentials)
	mux.HandleFunc("POST /instances/{id}/credentials", s.HandleBindCredential)
	mux.HandleFunc("DELETE /instances/{id}/credentials/{cred_id}", s.HandleUnbindCredential)
	mux.HandleFunc("GET /machines", s.HandleListMachines)
	mux.HandleFunc("POST /machines", s.HandleCreateMachine)
	mux.HandleFunc("GET /machines/{id}", s.HandleGetMachine)
	mux.HandleFunc("PUT /machines/{id}", s.HandleUpdateMachine)
	mux.HandleFunc("PUT /machines/{id}/computer-use", s.HandleSetMachineComputerUse)
	mux.HandleFunc("DELETE /machines/{id}", s.HandleDeleteMachine)
	mux.HandleFunc("GET /machines/stats", s.HandleMachineStatsOverview)
	mux.HandleFunc("GET /machines/{id}/stats/history", s.HandleMachineStatsHistory)
	mux.HandleFunc("GET /machines/{id}/disk-usage", s.HandleMachineDiskUsage)
	mux.HandleFunc("POST /machines/{id}/disk-usage/scans", s.HandleStartMachineDiskScan)
	mux.HandleFunc("GET /machines/{id}/disk-usage/entries", s.HandleMachineDiskEntries)
	mux.HandleFunc("POST /machines/{id}/disk-usage/deletion-checks", s.HandleCheckMachineDiskDeletion)
	mux.HandleFunc("POST /machines/{id}/disk-usage/deletions", s.HandleDeleteFromMachineDisk)
	mux.HandleFunc("GET /machines/{id}/disk-usage/deletions", s.HandleMachineDiskDeletions)
}
