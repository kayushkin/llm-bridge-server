package agentstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func hashContent(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, embedder: NewEmbedder()}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return s
}

func TestSeedAndResolve(t *testing.T) {
	s := testStore(t)

	// Create agent
	agent := &Agent{Slug: "test-warrior", DisplayName: "Test Warrior", Emoji: "⚔️", Projects: "proj1,proj2", Enabled: true}
	if err := s.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if agent.ID == 0 {
		t.Fatal("agent ID should be set")
	}

	// Create harnesses
	for _, o := range []Harness{
		{ID: "inber", DisplayName: "Inber"},
		{ID: "openclaw", DisplayName: "OpenClaw"},
	} {
		if err := s.UpsertHarness(&o); err != nil {
			t.Fatal(err)
		}
	}

	// Register under harnesses
	if err := s.UpsertAgentHarness(&AgentHarness{AgentID: agent.ID, HarnessID: "inber", HarnessAgentID: "test-warrior", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgentHarness(&AgentHarness{AgentID: agent.ID, HarnessID: "openclaw", HarnessAgentID: "test-project", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Add aliases
	if err := s.UpsertAlias(&AgentNameAlias{AgentID: agent.ID, Alias: "test-project", Context: "openclaw"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAlias(&AgentNameAlias{AgentID: agent.ID, Alias: "tw", Context: "dash"}); err != nil {
		t.Fatal(err)
	}

	// Resolve by slug
	a, err := s.ResolveAgentName("test-warrior")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != agent.ID {
		t.Fatalf("expected agent ID %d, got %d", agent.ID, a.ID)
	}

	// Resolve by alias "test-project"
	a2, err := s.ResolveAgentName("test-project")
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID != agent.ID {
		t.Fatalf("expected agent ID %d, got %d", agent.ID, a2.ID)
	}

	// Resolve by alias "tw"
	a3, err := s.ResolveAgentName("tw")
	if err != nil {
		t.Fatal(err)
	}
	if a3.ID != agent.ID {
		t.Fatalf("expected agent ID %d, got %d", agent.ID, a3.ID)
	}

	// GetAgentHarnesses
	harnesses, err := s.GetAgentHarnesses(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(harnesses) != 2 {
		t.Fatalf("expected 2 harnesses, got %d", len(harnesses))
	}

	// GetHarnessAgents("inber")
	inberAgents, err := s.GetHarnessAgents("inber")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ao := range inberAgents {
		if ao.AgentID == agent.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("agent not found in inber harness agents")
	}

	// GetHarnessAgents("openclaw")
	ocAgents, err := s.GetHarnessAgents("openclaw")
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, ao := range ocAgents {
		if ao.AgentID == agent.ID && ao.HarnessAgentID == "test-project" {
			found = true
		}
	}
	if !found {
		t.Fatal("agent not found in openclaw harness agents with correct harness_agent_id")
	}
}

func TestFileDistributionAndDriftDetection(t *testing.T) {
	s := testStore(t)

	agent := &Agent{Slug: "drift-agent", DisplayName: "Drift Agent", Enabled: true}
	if err := s.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber"}); err != nil {
		t.Fatal(err)
	}

	content := "You are a warrior spirit."
	hash := hashContent(content)
	nature := &AgentNature{AgentID: agent.ID, Kind: "identity", Content: content, ContentHash: hash}
	if err := s.UpsertAgentNature(nature); err != nil {
		t.Fatal(err)
	}

	// Record distribution
	distID, err := s.RecordDistribution(agent.ID, "inber", "/tmp/soul.md", hash, []int64{nature.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Scan with SAME hash → unchanged
	scanID1, err := s.RecordScan(distID, hash, "unchanged", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = scanID1

	// Scan with DIFFERENT hash → modified
	differentHash := hashContent("modified content")
	scanID2, err := s.RecordScan(distID, differentHash, "modified", "content changed")
	if err != nil {
		t.Fatal(err)
	}

	// GetModifiedFiles should return the modified scan
	modified, err := s.GetModifiedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(modified) != 1 {
		t.Fatalf("expected 1 modified file, got %d", len(modified))
	}
	if modified[0].ID != scanID2 {
		t.Fatalf("expected scan ID %d, got %d", scanID2, modified[0].ID)
	}

	// Mark ingested
	if err := s.MarkIngested(scanID2); err != nil {
		t.Fatal(err)
	}

	// GetModifiedFiles should now be empty
	modified, err = s.GetModifiedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(modified) != 0 {
		t.Fatalf("expected 0 modified files after ingestion, got %d", len(modified))
	}
}

func TestNatureUpdateTriggersDistribution(t *testing.T) {
	s := testStore(t)

	agent := &Agent{Slug: "nature-agent", DisplayName: "Nature Agent", Enabled: true}
	if err := s.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber"}); err != nil {
		t.Fatal(err)
	}

	// Create nature
	content1 := "Original identity"
	hash1 := hashContent(content1)
	nature := &AgentNature{AgentID: agent.ID, Kind: "identity", Content: content1, ContentHash: hash1}
	if err := s.UpsertAgentNature(nature); err != nil {
		t.Fatal(err)
	}

	// Distribute with hash1
	distID, err := s.RecordDistribution(agent.ID, "inber", "/tmp/soul.md", hash1, []int64{nature.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Update nature content
	content2 := "Updated identity"
	hash2 := hashContent(content2)
	nature.Content = content2
	nature.ContentHash = hash2
	if err := s.UpsertAgentNature(nature); err != nil {
		t.Fatal(err)
	}

	// Verify nature hash changed
	updated, err := s.GetAgentNature(nature.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ContentHash != hash2 {
		t.Fatalf("expected hash %s, got %s", hash2, updated.ContentHash)
	}

	// Record new distribution with new hash
	_, err = s.RecordDistribution(agent.ID, "inber", "/tmp/soul.md", hash2, []int64{nature.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Scan the OLD distribution with hash1 — file still has old content
	_, err = s.RecordScan(distID, hash1, "modified", "stale file")
	if err != nil {
		t.Fatal(err)
	}

	modified, err := s.GetModifiedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(modified) != 1 {
		t.Fatalf("expected 1 modified file, got %d", len(modified))
	}
}

func TestAgentDeletion(t *testing.T) {
	s := testStore(t)

	agent := &Agent{Slug: "delete-me", DisplayName: "Delete Me", Enabled: true}
	if err := s.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber"}); err != nil {
		t.Fatal(err)
	}

	// Add nature
	nature := &AgentNature{AgentID: agent.ID, Kind: "identity", Content: "test", ContentHash: hashContent("test")}
	if err := s.UpsertAgentNature(nature); err != nil {
		t.Fatal(err)
	}

	// Add alias
	if err := s.UpsertAlias(&AgentNameAlias{AgentID: agent.ID, Alias: "dm", Context: "dash"}); err != nil {
		t.Fatal(err)
	}

	// Add harness mapping
	if err := s.UpsertAgentHarness(&AgentHarness{AgentID: agent.ID, HarnessID: "inber", HarnessAgentID: "delete-me", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Add distribution
	if _, err := s.RecordDistribution(agent.ID, "inber", "/tmp/test.md", hashContent("test"), nil); err != nil {
		t.Fatal(err)
	}

	// Delete agent
	if err := s.DeleteAgent(agent.ID); err != nil {
		t.Fatal(err)
	}

	// Verify cascades
	natures, err := s.ListAgentNature(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(natures) != 0 {
		t.Fatalf("expected 0 natures, got %d", len(natures))
	}

	aliases, err := s.GetAgentAliases(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 0 {
		t.Fatalf("expected 0 aliases, got %d", len(aliases))
	}

	harnesses, err := s.GetAgentHarnesses(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(harnesses) != 0 {
		t.Fatalf("expected 0 harnesses, got %d", len(harnesses))
	}

	dists, err := s.ListDistributions(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dists) != 0 {
		t.Fatalf("expected 0 distributions, got %d", len(dists))
	}
}

func TestAgentToolsAndFullConfig(t *testing.T) {
	s := testStore(t)

	agent := &Agent{Slug: "warrior", DisplayName: "Warrior", Role: "fights things", Emoji: "⚔️", Enabled: true}
	if err := s.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber", DefaultAgentID: &agent.ID}); err != nil {
		t.Fatal(err)
	}

	ao := AgentHarness{
		AgentID: agent.ID, HarnessID: "inber", HarnessAgentID: "warrior",
		Enabled: true, ModelPrimary: "claude-opus-4-6", ThinkingBudget: 2048,
		ContextBudget: 50000, ContextTags: `["identity","code"]`,
		MaxTurns: 5, MaxInputTokens: 200000, MaxResponseTime: 20,
		Project: "warrior-project", WorkspacePath: "/home/test/warrior",
	}
	if err := s.UpsertAgentHarness(&ao); err != nil {
		t.Fatal(err)
	}

	// Set tools
	tools := []string{"shell", "read_file", "write_file"}
	if err := s.SetAgentTools(agent.ID, "inber", tools); err != nil {
		t.Fatal(err)
	}

	// List tools
	got, err := s.ListAgentTools(agent.ID, "inber")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(got))
	}

	// Add nature
	if err := s.UpsertAgentNature(&AgentNature{AgentID: agent.ID, Kind: "identity", Content: "soul content", ContentHash: hashContent("soul content")}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgentNature(&AgentNature{AgentID: agent.ID, Kind: "principle", Content: "principles", ContentHash: hashContent("principles")}); err != nil {
		t.Fatal(err)
	}

	// Get full config
	cfg, err := s.GetFullAgentConfig("warrior", "inber")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "claude-opus-4-6" {
		t.Fatalf("expected model claude-opus-4-6, got %s", cfg.Model)
	}
	if cfg.Thinking != 2048 {
		t.Fatalf("expected thinking 2048, got %d", cfg.Thinking)
	}
	if len(cfg.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(cfg.Tools))
	}
	if cfg.ContextBudget != 50000 {
		t.Fatalf("expected context budget 50000, got %d", cfg.ContextBudget)
	}
	if cfg.Soul != "soul content" {
		t.Fatalf("expected soul content, got %q", cfg.Soul)
	}
	if cfg.Principles != "principles" {
		t.Fatalf("expected principles, got %q", cfg.Principles)
	}
	if cfg.MaxTurns != 5 {
		t.Fatalf("expected max turns 5, got %d", cfg.MaxTurns)
	}

	// GetAllAgentConfigs
	configs, defaultSlug, err := s.GetAllAgentConfigs("inber")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if defaultSlug != "warrior" {
		t.Fatalf("expected default warrior, got %s", defaultSlug)
	}

	// Replace tools
	if err := s.SetAgentTools(agent.ID, "inber", []string{"shell"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListAgentTools(agent.ID, "inber")
	if len(got) != 1 {
		t.Fatalf("expected 1 tool after replace, got %d", len(got))
	}
}

func TestSystemPromptRefs(t *testing.T) {
	s := testStore(t)

	// Create 3 agents
	parent := &Agent{Slug: "harness", DisplayName: "Harness", Enabled: true}
	sub1 := &Agent{Slug: "sub-agent-1", DisplayName: "Sub 1", Enabled: true}
	sub2 := &Agent{Slug: "sub-agent-2", DisplayName: "Sub 2", Enabled: true}
	for _, a := range []*Agent{parent, sub1, sub2} {
		if err := s.UpsertAgent(a); err != nil {
			t.Fatal(err)
		}
	}

	// Create harness entry
	if err := s.UpsertHarness(&Harness{ID: "inber", DisplayName: "Inber"}); err != nil {
		t.Fatal(err)
	}

	// Add system prompt refs
	for _, ref := range []*AgentSystemPromptRef{
		{HarnessID: "inber", HostAgentID: parent.ID, ReferencedAgentID: sub1.ID, PromptLocation: "AGENTS.md"},
		{HarnessID: "inber", HostAgentID: parent.ID, ReferencedAgentID: sub2.ID, PromptLocation: "AGENTS.md"},
	} {
		if err := s.UpsertSystemPromptRef(ref); err != nil {
			t.Fatal(err)
		}
	}

	// List refs
	refs, err := s.ListSystemPromptRefs(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}

	// Delete sub1 → cascade should remove its ref
	if err := s.DeleteAgent(sub1.ID); err != nil {
		t.Fatal(err)
	}

	refs, err = s.ListSystemPromptRefs(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref after deletion, got %d", len(refs))
	}
	if refs[0].ReferencedAgentID != sub2.ID {
		t.Fatalf("expected remaining ref to sub2 (ID=%d), got %d", sub2.ID, refs[0].ReferencedAgentID)
	}
}
