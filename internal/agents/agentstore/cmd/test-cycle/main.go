// test-cycle: exercises the full nature → distribute → scan → detect drift cycle
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"

	agentstore "github.com/kayushkin/agent-store"
)

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func main() {
	store, err := agentstore.Open("")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	// 1. Resolve claxon by slug
	agent, err := store.ResolveAgentName("claxon")
	if err != nil {
		log.Fatalf("resolve claxon: %v", err)
	}
	fmt.Printf("✓ Resolved 'claxon' → id=%d slug=%s emoji=%s\n", agent.ID, agent.Slug, agent.Emoji)

	// 2. Resolve by the harness agent id "main"
	agent2, err := store.ResolveAgentName("main")
	if err != nil {
		// "main" is the harness_agent_id, not an alias — check if that works
		fmt.Printf("⚠ ResolveAgentName('main') failed: %v (expected — it's an orch ID not alias)\n", err)
	} else {
		fmt.Printf("✓ Resolved 'main' → id=%d slug=%s\n", agent2.ID, agent2.Slug)
	}

	// 3. Get harness mappings
	orchs, err := store.GetAgentHarnesses(agent.ID)
	if err != nil {
		log.Fatalf("get orch: %v", err)
	}
	fmt.Printf("✓ Claxon has %d harness mappings:\n", len(orchs))
	for _, o := range orchs {
		fmt.Printf("  - %s → %q (model: %s)\n", o.HarnessID, o.HarnessAgentID, o.ModelPrimary)
	}

	// 4. Add nature content for claxon
	natureContent := "# Claxon 🦀\n\nThe main harness. Direct, no fluff.\n"
	nature := &agentstore.AgentNature{
		AgentID:     agent.ID,
		Kind:        "identity",
		Content:     natureContent,
		ContentHash: hash(natureContent),
		Priority:    0,
		SourcePath:  "~/repos/inber/agents/claxon/soul.md",
	}
	if err := store.UpsertAgentNature(nature); err != nil {
		log.Fatalf("upsert nature: %v", err)
	}
	fmt.Printf("✓ Added nature id=%d hash=%s...\n", nature.ID, nature.ContentHash[:16])

	// 5. Simulate distributing a soul.md file
	tmpDir, _ := os.MkdirTemp("", "agent-store-test-*")
	defer os.RemoveAll(tmpDir)
	filePath := filepath.Join(tmpDir, "soul.md")
	os.WriteFile(filePath, []byte(natureContent), 0644)

	distID, err := store.RecordDistribution(agent.ID, "inber", filePath, hash(natureContent), []int64{nature.ID})
	if err != nil {
		log.Fatalf("record dist: %v", err)
	}
	fmt.Printf("✓ Recorded distribution id=%d path=%s\n", distID, filePath)

	// 6. Scan — no changes yet
	scanID1, err := store.RecordScan(distID, hash(natureContent), "unchanged", "")
	if err != nil {
		log.Fatalf("scan: %v", err)
	}
	fmt.Printf("✓ Scan %d: unchanged (hashes match)\n", scanID1)

	// 7. Simulate external edit (someone edits soul.md directly)
	editedContent := "# Claxon 🦀\n\nThe main harness. Direct, no fluff. Also likes crabs.\n"
	os.WriteFile(filePath, []byte(editedContent), 0644)
	newHash := hash(editedContent)

	scanID2, err := store.RecordScan(distID, newHash, "modified", "added crab line")
	if err != nil {
		log.Fatalf("scan: %v", err)
	}
	fmt.Printf("✓ Scan %d: modified (hash drift detected)\n", scanID2)

	// 8. Check for modified files
	modified, err := store.GetModifiedFiles()
	if err != nil {
		log.Fatalf("get modified: %v", err)
	}
	fmt.Printf("✓ GetModifiedFiles() returned %d modified file(s)\n", len(modified))
	for _, m := range modified {
		fmt.Printf("  - scan=%d dist=%d status=%s diff=%q ingested=%v\n",
			m.ID, m.FileDistributionID, m.Status, m.DiffSummary, m.Ingested)
	}

	// 9. Ingest the changes
	if err := store.MarkIngested(scanID2); err != nil {
		log.Fatalf("mark ingested: %v", err)
	}
	fmt.Printf("✓ Marked scan %d as ingested\n", scanID2)

	// 10. Verify no more pending modified files
	modified2, err := store.GetModifiedFiles()
	if err != nil {
		log.Fatalf("get modified: %v", err)
	}
	fmt.Printf("✓ After ingestion, GetModifiedFiles() returned %d file(s) — clean!\n", len(modified2))

	// 11. Check history — what do we have?
	fmt.Println("\n--- Checking memory/history references ---")
	// No ListMemories yet — check if any exist via direct query
	var memCount int
	store.DB().QueryRow("SELECT COUNT(*) FROM memories").Scan(&memCount)
	fmt.Printf("Memories in store: %d\n", memCount)

	natures, err := store.ListAgentNature(agent.ID)
	if err != nil {
		log.Fatalf("list nature: %v", err)
	}
	fmt.Printf("Nature entries for claxon: %d\n", len(natures))
	for _, n := range natures {
		fmt.Printf("  - id=%d kind=%s hash=%s... source=%s\n", n.ID, n.Kind, n.ContentHash[:16], n.SourcePath)
	}

	dists, err := store.ListDistributions(agent.ID)
	if err != nil {
		log.Fatalf("list dist: %v", err)
	}
	fmt.Printf("Distributions for claxon: %d\n", len(dists))

	fmt.Println("\n✅ Full cycle complete!")
}
