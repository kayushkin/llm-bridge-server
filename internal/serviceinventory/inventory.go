package serviceinventory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// healthcheckStatus is the part of healthcheck's GET /api/status this package
// reads. Field names follow healthcheck's checker.ServiceState.
type healthcheckStatus struct {
	Timestamp string `json:"timestamp"`
	Services  []struct {
		Name         string  `json:"name"`
		Type         string  `json:"type"`
		Status       string  `json:"status"`
		ResponseMs   int64   `json:"response_ms"`
		LastCheck    string  `json:"last_check"`
		LastError    string  `json:"last_error"`
		UptimePct24h float64 `json:"uptime_pct_24h"`
		EnabledState string  `json:"enabled_state"`
		Unit         string  `json:"unit"`
		SystemUnit   bool    `json:"system_unit"`
		URL          string  `json:"url"`
	} `json:"services"`
}

// Inventory is one reading of the host: healthcheck's status joined to the
// processes and database files behind each service.
type Inventory struct {
	Response  msg.ServiceInventoryResponse
	Databases map[string]bool // every path listed under any service
	ReadAt    time.Time
}

// Read asks healthcheck for its status and joins each service to its
// processes and open databases.
func Read(ctx context.Context, client *http.Client, healthcheckURL string) (*Inventory, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthcheckURL+"/api/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("healthcheck %s: %w", healthcheckURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("healthcheck %s: HTTP %d: %s", healthcheckURL, resp.StatusCode, body)
	}
	var status healthcheckStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("healthcheck %s: decode status: %w", healthcheckURL, err)
	}

	sort.Slice(status.Services, func(i, j int) bool { return status.Services[i].Name < status.Services[j].Name })
	inv := &Inventory{Databases: map[string]bool{}, ReadAt: time.Now()}
	inv.Response = msg.ServiceInventoryResponse{CheckedAt: status.Timestamp, HealthcheckURL: healthcheckURL, Services: []msg.ServiceInventoryEntry{}}
	holders := map[string][]string{} // database path → service names
	for _, s := range status.Services {
		entry := msg.ServiceInventoryEntry{
			Name: s.Name, Type: s.Type, Status: s.Status, ResponseMs: s.ResponseMs, LastCheck: s.LastCheck,
			LastError: s.LastError, UptimePct24h: s.UptimePct24h, EnabledState: s.EnabledState,
			Unit: s.Unit, SystemUnit: s.SystemUnit, URL: s.URL,
			PIDs: []int{}, Databases: []msg.ServiceDatabase{},
		}
		pids, err := processesFor(s.Type, s.Unit, s.SystemUnit, s.URL)
		if err != nil {
			entry.ProcessLookupError = err.Error()
		} else if len(pids) > 0 {
			entry.PIDs = pids
			paths, err := OpenDatabasePaths(pids)
			if err != nil {
				entry.ProcessLookupError = err.Error()
			}
			for _, p := range paths {
				db := msg.ServiceDatabase{Path: p, SharedWith: []string{}}
				if info, err := os.Stat(p); err == nil {
					db.SizeBytes = info.Size()
					db.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
				}
				entry.Databases = append(entry.Databases, db)
				holders[p] = append(holders[p], s.Name)
				inv.Databases[p] = true
			}
		}
		inv.Response.Services = append(inv.Response.Services, entry)
	}
	for i := range inv.Response.Services {
		for j, db := range inv.Response.Services[i].Databases {
			for _, holder := range holders[db.Path] {
				if holder != inv.Response.Services[i].Name {
					inv.Response.Services[i].Databases[j].SharedWith = append(inv.Response.Services[i].Databases[j].SharedWith, holder)
				}
			}
		}
	}
	return inv, nil
}

// processesFor finds the pids behind one healthcheck entry. A command check
// has none by construction.
func processesFor(checkType, unit string, systemUnit bool, rawURL string) ([]int, error) {
	switch checkType {
	case "systemd":
		if unit == "" {
			return nil, fmt.Errorf("systemd check names no unit")
		}
		return PIDsForUnit(unit, systemUnit)
	case "http":
		port, err := ListeningPortOf(rawURL)
		if err != nil {
			return nil, err
		}
		return PIDsListeningOn(port)
	case "command":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown check type %q", checkType)
}
