package skillstore

// DefaultSources is the canonical initial registry of upstream skill sources,
// which the standalone store's seed command registered on a fresh install.
// Extend it as new upstream repositories are vetted. Each call returns a fresh
// copy, so a caller may upsert the entries (which sets their ids) freely.
func DefaultSources() []Source {
	return []Source{
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
}
