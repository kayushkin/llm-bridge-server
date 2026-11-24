// Migration tool: inber agents.json + soul.md files → agent-store database
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/agent-store/internal/config"
	"github.com/kayushkin/agent-store/internal/textutil"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

type inberAgentsFile struct {
	Default string                       `json:"default"`
	Agents  map[string]*inberAgentConfig `json:"agents"`
}

type inberAgentConfig struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Model string `json:"model"`
}

// plannedAgent is what main() derives from one agents.json entry before it
// touches the database: the whole read-only half of the migration loop.
//
// It is split out so the derivation can be tested without a store, a filesystem
// or an inber checkout. Testing textutil.UpperFirstRune on its own does not
// cover this — the helper is not where either of this file's two panics lived.
type plannedAgent struct {
	Slug        string
	DisplayName string
	Role        string
	Model       string
}

// planMigration derives the agent rows main() upserts, in slug order.
//
// It refuses the whole file rather than returning a partial plan when an entry's
// config is null. That case is reachable from a hand-edited agents.json —
// `{"agents": {"ghost": null}}` parses without error into a nil map value — and
// every use of it in the migration loop (Name, Role, Model) is an unguarded
// dereference. Rejecting it here keeps the loop below free of nil checks and
// matches how this tool already treats every other malformed input: say what is
// wrong and exit non-zero.
//
// The slug order is deliberate. Ranging a map directly, as this loop used to,
// migrates agents in a different order on every run, which makes the tool's
// output impossible to diff against a previous run.
func planMigration(af inberAgentsFile) ([]plannedAgent, error) {
	var nullConfigs []string
	for id, cfg := range af.Agents {
		if cfg == nil {
			nullConfigs = append(nullConfigs, id)
		}
	}
	if len(nullConfigs) > 0 {
		sort.Strings(nullConfigs)
		return nil, fmt.Errorf("agents.json has null config for: %s", strings.Join(nullConfigs, ", "))
	}

	planned := make([]plannedAgent, 0, len(af.Agents))
	for id, cfg := range af.Agents {
		planned = append(planned, plannedAgent{
			Slug:        id,
			DisplayName: displayNameFor(id, cfg.Name),
			Role:        cfg.Role,
			Model:       cfg.Model,
		})
	}
	sort.Slice(planned, func(i, j int) bool { return planned[i].Slug < planned[j].Slug })
	return planned, nil
}

// displayNameFor picks the name shown for an agent: the one configured in
// agents.json, or the agent's own id with its first rune upper-cased when no
// name is configured.
//
// The id comes in as a JSON object key, so it is arbitrary text. An empty key
// and a key whose first letter is multi-byte are both legal JSON and both broke
// the byte-indexed spelling this replaced.
func displayNameFor(id, configuredName string) string {
	if configuredName != "" {
		return configuredName
	}
	return textutil.UpperFirstRune(id)
}

func main() {
	settings, err := config.NewMigrateInberCommandSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	inberPath := settings.String(config.SettingInberRepositoryPath)

	store, err := agentstore.Open("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open agent-store: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	agentsPath := filepath.Join(inberPath, "agents.json")
	data, err := os.ReadFile(agentsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", agentsPath, err)
		os.Exit(1)
	}

	var af inberAgentsFile
	if err := json.Unmarshal(data, &af); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse agents.json: %v\n", err)
		os.Exit(1)
	}

	// Ensure inber harness
	orch := agentstore.Harness{
		ID:          "inber",
		DisplayName: "Inber",
		ConfigPath:  filepath.Join(inberPath, "agents"),
	}
	if err := store.UpsertHarness(&orch); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create harness: %v\n", err)
		os.Exit(1)
	}

	agentsDir := filepath.Join(inberPath, "agents")

	planned, err := planMigration(af)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read agents: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Migrating %d agents...\n", len(planned))
	for _, p := range planned {
		id := p.Slug

		agent := agentstore.Agent{
			Slug:        id,
			DisplayName: p.DisplayName,
			Role:        p.Role,
			Enabled:     true,
		}
		if err := store.UpsertAgent(&agent); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to upsert agent %s: %v\n", id, err)
			os.Exit(1)
		}

		// Agent→harness mapping
		ao := agentstore.AgentHarness{
			AgentID:             agent.ID,
			HarnessID:      "inber",
			HarnessAgentID: id,
			Enabled:             true,
			ModelPrimary:        p.Model,
		}
		if err := store.UpsertAgentHarness(&ao); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to map agent %s: %v\n", id, err)
			os.Exit(1)
		}

		// Import soul.md as nature
		soulPath := filepath.Join(agentsDir, id, "soul.md")
		if content, err := os.ReadFile(soulPath); err == nil {
			n := agentstore.AgentNature{
				AgentID:    agent.ID,
				Kind:       "identity",
				Content:    string(content),
				Priority:   0,
				SourcePath: soulPath,
			}
			if err := store.UpsertAgentNature(&n); err != nil {
				fmt.Printf("    Warning: could not save nature for %s: %v\n", id, err)
			}
		}

		fmt.Printf("  - %s (id=%d)\n", id, agent.ID)
	}

	// Set default agent
	if af.Default != "" {
		defaultAgent, err := store.GetAgentBySlug(af.Default)
		if err == nil {
			orch.DefaultAgentID = agentstore.Int64Ptr(defaultAgent.ID)
			_ = store.UpsertHarness(&orch)
		}
	}

	fmt.Println("\nMigration complete!")
}
