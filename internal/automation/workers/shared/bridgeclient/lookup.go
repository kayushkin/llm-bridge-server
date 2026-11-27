// Package bridgeclient answers the two questions a board setting leaves open
// when a scheduler binary spawns a session from it.
//
// kanban-store stores a board's default_instance_id and default_agent_id as
// the ids their owners hand out — the instance id llm-bridge-server lists, and
// agent-store's numeric agent id — because ids are the only thing that survive
// a rename. llm-bridge-server's POST /sessions, though, takes the harness type
// alongside the instance and takes the agent by slug, so a dispatcher has to
// resolve both at spawn time. Resolving here, against the bridge, rather than
// carrying a copy of either answer, is what keeps a renamed agent or a retyped
// instance from silently spawning the wrong thing.
package bridgeclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// InstanceHarnessType reads GET /instances/{id} and returns the harness type
// the instance runs. An unknown instance is an error naming it, never an
// empty harness: the bridge would refuse the spawn anyway, and a caller that
// got "" back could not tell the two apart.
func InstanceHarnessType(bridgeURL, instanceID string) (msg.Harness, error) {
	if instanceID == "" {
		return "", fmt.Errorf("bridgeclient: no instance id to look up")
	}
	var instance msg.Instance
	if err := getJSON(strings.TrimRight(bridgeURL, "/")+"/instances/"+instanceID, &instance); err != nil {
		return "", fmt.Errorf("instance %s: %w", instanceID, err)
	}
	if instance.HarnessType == "" {
		return "", fmt.Errorf("instance %s: llm-bridge-server lists it with no harness_type", instanceID)
	}
	return instance.HarnessType, nil
}

// AgentSlugByID reads GET /agents and returns the slug of the agent whose
// numeric id, rendered as a string, equals agentID — the form kanban-store
// stores a board's default_agent_id in. The slug is what CreateSessionRequest
// .AgentID takes. An id no agent carries is an error naming it.
func AgentSlugByID(bridgeURL, agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("bridgeclient: no agent id to look up")
	}
	var agents []struct {
		ID   json.Number `json:"id"`
		Slug string      `json:"slug"`
	}
	if err := getJSON(strings.TrimRight(bridgeURL, "/")+"/agents", &agents); err != nil {
		return "", fmt.Errorf("agents: %w", err)
	}
	for _, agent := range agents {
		if agent.ID.String() == agentID {
			if agent.Slug == "" {
				return "", fmt.Errorf("agent %s has no slug on llm-bridge-server, and a session is started by slug", agentID)
			}
			return agent.Slug, nil
		}
	}
	return "", fmt.Errorf("agent id %s is not in llm-bridge-server's GET /agents", agentID)
}

func getJSON(url string, out any) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode reply from %s: %w", url, err)
	}
	return nil
}
