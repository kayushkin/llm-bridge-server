// Command seed registers the canonical upstream skill sources and syncs them.
//
//	go run ./cmd/seed            # register + sync all
//	go run ./cmd/seed -list      # list sources + skill counts
//	go run ./cmd/seed -sync NAME # sync one source
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/kayushkin/llm-bridge/servicesettings"
	skillstore "github.com/kayushkin/skill-store"
)

// seedSources is the canonical initial registry.
// Extend this list as new upstream repos are vetted.
var seedSources = []skillstore.Source{
	{
		Name:      "anthropics",
		RepoURL:   "https://github.com/anthropics/skills.git",
		Branch:    "main",
		Subpath:   "", // entire repo is skill dirs under a top-level folder
		TrustTier: "official",
		License:   "", // Anthropic ships no LICENSE file; treat as sample code
		Enabled:   true,
	},
	{
		Name:      "antigravity",
		RepoURL:   "https://github.com/sickn33/antigravity-awesome-skills.git",
		Branch:    "main",
		Subpath:   "skills",
		TrustTier: "community",
		License:   "MIT",
		Enabled:   true,
	},
}

func main() {
	list := flag.Bool("list", false, "list sources and exit")
	syncOne := flag.String("sync", "", "sync only the named source")
	noSync := flag.Bool("no-sync", false, "register sources but do not sync")
	flag.Parse()

	// The same declarations the server reads, so this command writes into the
	// directory the server will serve from, and refuses the same misspellings.
	// It mounts no route: GET /settings is the server's.
	settings, err := skillstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}

	store, err := skillstore.Open(settings.String(skillstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if *list {
		srcs, err := store.ListSources()
		if err != nil {
			log.Fatal(err)
		}
		for _, s := range srcs {
			fmt.Printf("%-14s  %-6s  %-9s  skills=%d  %s\n", s.Name, s.Branch, s.TrustTier, s.LastSkillCount, s.RepoURL)
		}
		return
	}

	// Register seed sources (idempotent upserts)
	for i := range seedSources {
		src := seedSources[i]
		if err := store.UpsertSource(&src); err != nil {
			log.Fatalf("upsert %s: %v", src.Name, err)
		}
		fmt.Printf("registered  %s  (id=%d, tier=%s)\n", src.Name, src.ID, src.TrustTier)
	}

	if *noSync {
		return
	}

	srcs, err := store.ListSources()
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range srcs {
		if *syncOne != "" && s.Name != *syncOne {
			continue
		}
		fmt.Printf("syncing     %s ...\n", s.Name)
		res, err := store.Sync(s.ID)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			continue
		}
		fmt.Printf("  found=%d inserted=%d updated=%d pruned=%d commit=%s\n",
			res.Found, res.Inserted, res.Updated, res.Pruned, shortSHA(res.Commit))
		for _, e := range res.Errors {
			fmt.Printf("  WARN: %s\n", e)
		}
	}
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
