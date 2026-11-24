// Command seed populates the agent-store DB from mapping.json + inber agents.json + soul files.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/agent-store/internal/config"
	"github.com/kayushkin/agent-store/internal/textutil"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// mapping.json structures
type MappingFile struct {
	Agents []MappingAgent `json:"agents"`
}

type MappingAgent struct {
	ID      string  `json:"id"`
	Inber   *string `json:"inber"`
	Project string  `json:"project"`
	Emoji   string  `json:"emoji"`
}

// agents.json structures
type InberConfig struct {
	Default string                `json:"default"`
	Agents  map[string]InberAgent `json:"agents"`
}

type InberAgent struct {
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Model    string   `json:"model"`
	Thinking int      `json:"thinking"`
	Tools    []string `json:"tools"`
	Context  struct {
		Tags   []string `json:"tags"`
		Budget int      `json:"budget"`
	} `json:"context"`
	Limits struct {
		MaxTurns        int `json:"maxTurns"`
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxResponseTime int `json:"maxResponseTime"`
	} `json:"limits"`
	Workspace string   `json:"workspace"`
	Project   string   `json:"project"`
	Projects  []string `json:"projects"`
	System    string   `json:"system"`
	Shelved   bool     `json:"shelved"`
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// agentIdentity is the part of an agent row that seed derives from config
// rather than reading straight out of it: the three fields where mapping.json
// and inber's agents.json can each supply a value.
//
// Split out of main()'s loop so it can be tested without a database, an inber
// checkout or a mapping file. The helper it calls, textutil.UpperFirstRune, has
// its own tests; those cannot see this, because the defect this file carried
// was in how the id reached the helper, not in the helper.
type agentIdentity struct {
	DisplayName string
	Role        string
	Projects    string
}

// inberSlugFor returns the agents.json key a mapping.json entry points at, and
// whether it points at one at all. A mapping entry with no "inber" field is not
// an inber agent and must not be looked up: the zero slug would match an entry
// keyed by the empty string if agents.json ever grew one.
func inberSlugFor(ma MappingAgent) (string, bool) {
	if ma.Inber == nil {
		return "", false
	}
	return *ma.Inber, true
}

// resolveAgentIdentity derives an agent's display name, role and projects from
// its mapping.json entry, letting a matching inber agents.json entry override
// all three.
//
// The display name falls back to the agent's own id with its first rune
// upper-cased. ids come from mapping.json, so they are arbitrary text: "" and a
// multi-byte first letter are both legal and both broke the byte-indexed
// spelling this replaced.
func resolveAgentIdentity(ma MappingAgent, ia InberAgent, hasInber bool) agentIdentity {
	identity := agentIdentity{
		DisplayName: textutil.UpperFirstRune(ma.ID),
		Projects:    ma.Project,
	}
	if !hasInber {
		return identity
	}

	// Note: this assigns ia.Name unconditionally, so an inber entry with no
	// name blanks a display name that was already derived from the id. That is
	// this tool's behavior today and is pinned as such in the tests, not
	// endorsed; cmd/migrate-inber guards the same override with a != "" check.
	// Reconciling the two is filed separately.
	identity.DisplayName = ia.Name
	identity.Role = ia.Role
	if len(ia.Projects) > 0 {
		identity.Projects = strings.Join(ia.Projects, ",")
	} else if ia.Project != "" {
		identity.Projects = ia.Project
	}
	return identity
}

func main() {
	settings, err := config.NewSeedCommandSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatal(err)
	}
	dbPath := settings.String(config.SettingDatabasePath)

	repoRoot := filepath.Dir(filepath.Dir(os.Args[0]))
	// Try to find repo root from working directory
	if _, err := os.Stat("agents/mapping.json"); err == nil {
		repoRoot = "."
	}

	mappingPath := filepath.Join(repoRoot, "agents/mapping.json")
	if len(os.Args) > 1 {
		mappingPath = os.Args[1]
	}

	inberRoot := settings.String(config.SettingInberRepositoryPath)

	// Read mapping.json
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		log.Fatalf("read mapping: %v", err)
	}
	var mapping MappingFile
	if err := json.Unmarshal(data, &mapping); err != nil {
		log.Fatalf("parse mapping: %v", err)
	}

	// Read inber agents.json
	inberData, err := os.ReadFile(filepath.Join(inberRoot, "agents.json"))
	if err != nil {
		log.Fatalf("read agents.json: %v", err)
	}
	var inberCfg InberConfig
	if err := json.Unmarshal(inberData, &inberCfg); err != nil {
		log.Fatalf("parse agents.json: %v", err)
	}

	// Read shared identity files
	principles := readFile(filepath.Join(inberRoot, "agents/_principles.md"))
	values := readFile(filepath.Join(inberRoot, "agents/_values.md"))
	userCtx := readFile(filepath.Join(inberRoot, "agents/_user.md"))

	store, err := agentstore.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Seed harnesses
	orchs := []agentstore.Harness{
		{ID: "inber", DisplayName: "Inber", ConfigPath: "~/repos/inber/agents/"},
		{ID: "dash", DisplayName: "Dash", APIEndpoint: "localhost:8101/api/agents"},
	}
	for i := range orchs {
		if err := store.UpsertHarness(&orchs[i]); err != nil {
			log.Fatalf("upsert harness %s: %v", orchs[i].ID, err)
		}
		fmt.Printf("  harness: %s\n", orchs[i].ID)
	}

	agentIDs := map[string]int64{}

	for _, ma := range mapping.Agents {
		// Override from inber config if present
		inberSlug, pointsAtInber := inberSlugFor(ma)
		var ia InberAgent
		hasInber := false
		if pointsAtInber {
			ia, hasInber = inberCfg.Agents[inberSlug]
		}
		identity := resolveAgentIdentity(ma, ia, hasInber)

		agent := agentstore.Agent{
			Slug:        ma.ID,
			DisplayName: identity.DisplayName,
			Emoji:       ma.Emoji,
			Projects:    identity.Projects,
			Role:        identity.Role,
			Enabled:     true,
		}
		if err := store.UpsertAgent(&agent); err != nil {
			log.Fatalf("upsert agent %s: %v", ma.ID, err)
		}
		agentIDs[ma.ID] = agent.ID
		fmt.Printf("  agent: %s %s (id=%d)\n", ma.Emoji, ma.ID, agent.ID)

		// Inber harness config
		if hasInber {
			tagsJSON, _ := json.Marshal(ia.Context.Tags)
			ao := agentstore.AgentHarness{
				AgentID:         agent.ID,
				HarnessID:       "inber",
				HarnessAgentID:  inberSlug,
				Enabled:         true,
				ModelPrimary:    ia.Model,
				WorkspacePath:   ia.Workspace,
				ThinkingBudget:  ia.Thinking,
				ContextBudget:   ia.Context.Budget,
				ContextTags:     string(tagsJSON),
				MaxTurns:        ia.Limits.MaxTurns,
				MaxInputTokens:  ia.Limits.MaxInputTokens,
				MaxResponseTime: ia.Limits.MaxResponseTime,
				SystemPrompt:    ia.System,
				Project:         ia.Project,
				Shelved:         ia.Shelved,
			}
			if err := store.UpsertAgentHarness(&ao); err != nil {
				log.Fatalf("upsert ao inber/%s: %v", ma.ID, err)
			}

			// Set tools
			if len(ia.Tools) > 0 {
				if err := store.SetAgentTools(agent.ID, "inber", ia.Tools); err != nil {
					log.Fatalf("set tools inber/%s: %v", ma.ID, err)
				}
				fmt.Printf("    tools: %v\n", ia.Tools)
			}

			// Soul file
			soulPath := filepath.Join(inberRoot, "agents", inberSlug, "soul.md")
			soul := readFile(soulPath)
			if soul != "" {
				if err := upsertNature(store, agent.ID, "identity", soul, soulPath); err != nil {
					log.Fatalf("upsert soul %s: %v", ma.ID, err)
				}
			}

			// Shared nature (principles, values, user) — only for inber agents
			if principles != "" {
				upsertNature(store, agent.ID, "principle", principles, filepath.Join(inberRoot, "agents/_principles.md"))
			}
			if values != "" {
				upsertNature(store, agent.ID, "value", values, filepath.Join(inberRoot, "agents/_values.md"))
			}
			if userCtx != "" {
				upsertNature(store, agent.ID, "user", userCtx, filepath.Join(inberRoot, "agents/_user.md"))
			}
		}

	}

	// Set default agents
	defaults := map[string]string{"inber": "claxon", "dash": "oisin"}
	for orchID, agentSlug := range defaults {
		if id, ok := agentIDs[agentSlug]; ok {
			o, err := store.GetHarness(orchID)
			if err != nil {
				continue
			}
			o.DefaultAgentID = agentstore.Int64Ptr(id)
			store.UpsertHarness(o)
		}
	}

	fmt.Printf("\nSeeded %d agents across %d harnesses.\n", len(mapping.Agents), len(orchs))
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func upsertNature(store *agentstore.Store, agentID int64, kind, content, sourcePath string) error {
	// Check if nature already exists for this agent+kind+source
	natures, _ := store.ListAgentNature(agentID)
	for _, n := range natures {
		if n.Kind == kind && n.SourcePath == sourcePath {
			n.Content = content
			n.ContentHash = hash(content)
			return store.UpsertAgentNature(&n)
		}
	}
	n := &agentstore.AgentNature{
		AgentID:     agentID,
		Kind:        kind,
		Content:     content,
		ContentHash: hash(content),
		SourcePath:  sourcePath,
	}
	return store.UpsertAgentNature(n)
}
