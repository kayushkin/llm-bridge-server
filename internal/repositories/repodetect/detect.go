// Package repodetect is the single source of the repository-signature ruleset:
// a pure, dependency-light Detect(path) → Signature function plus tag helpers.
// It is split out from the repo-store service package so lean consumers (inber,
// the llm-bridge-server resolver, …) can import the ruleset without pulling in
// the SQLite store or HTTP server.
package repodetect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Signature is the structured result of inspecting a repository's manifests.
type Signature struct {
	Languages  []string `json:"languages"`
	Frameworks []string `json:"frameworks"`
	BuildTools []string `json:"build_tools"`
	TestTools  []string `json:"test_tools"`
}

// Tags flattens the signature into a sorted, deduped tag list. Frontend
// frameworks additionally roll up to the generic "web"/"frontend" tags so a
// bundle can match either the specific framework or the broad category.
func (s Signature) Tags() []string {
	var all []string
	all = append(all, s.Languages...)
	all = append(all, s.Frameworks...)
	all = append(all, s.BuildTools...)
	all = append(all, s.TestTools...)
	for _, f := range s.Frameworks {
		switch f {
		case "react", "vue", "svelte", "angular", "next":
			all = append(all, "web", "frontend")
		}
	}
	return DedupeSorted(all)
}

// PrimaryLanguage returns the single language a command-oriented consumer
// (build/format/test selection) should treat as primary, or "" if none. Order
// of preference is fixed and deterministic: go, rust, python, node. This lets a
// caller that needs one answer (inber's workflow hooks) derive it from the full
// signature without losing the multi-language detail.
func (s Signature) PrimaryLanguage() string {
	for _, want := range []string{"go", "rust", "python", "node"} {
		for _, l := range s.Languages {
			if l == want {
				return want
			}
		}
	}
	return ""
}

// Detect inspects repoPath's manifests and returns its signature. Unlike a
// first-match detector, it collects EVERY applicable signal: a repo with both
// go.mod and package.json (e.g. a Go service with a React frontend) reports
// both languages. Missing manifests are simply absent, never an error. A path
// that does not exist returns the underlying os error.
func Detect(repoPath string) (Signature, error) {
	var sig Signature
	fi, err := os.Stat(repoPath)
	if err != nil {
		return sig, err
	}
	if !fi.IsDir() {
		return sig, fmt.Errorf("repodetect: %s is not a directory", repoPath)
	}

	if exists(filepath.Join(repoPath, "go.mod")) {
		sig.Languages = append(sig.Languages, "go")
	}
	if exists(filepath.Join(repoPath, "Cargo.toml")) {
		sig.Languages = append(sig.Languages, "rust")
	}
	if exists(filepath.Join(repoPath, "pyproject.toml")) ||
		exists(filepath.Join(repoPath, "requirements.txt")) ||
		exists(filepath.Join(repoPath, "setup.py")) {
		sig.Languages = append(sig.Languages, "python")
	}
	if pkgPath := filepath.Join(repoPath, "package.json"); exists(pkgPath) {
		sig.Languages = append(sig.Languages, "node")
		detectNode(pkgPath, repoPath, &sig)
	}
	if exists(filepath.Join(repoPath, "tsconfig.json")) {
		sig.Languages = append(sig.Languages, "typescript")
	}
	if exists(filepath.Join(repoPath, "Dockerfile")) {
		sig.BuildTools = append(sig.BuildTools, "docker")
	}

	sig.Languages = DedupeSorted(sig.Languages)
	sig.Frameworks = DedupeSorted(sig.Frameworks)
	sig.BuildTools = DedupeSorted(sig.BuildTools)
	sig.TestTools = DedupeSorted(sig.TestTools)
	return sig, nil
}

// detectNode parses a package.json and augments sig with the Node-ecosystem
// frameworks, build tools, and test tools it can infer from declared deps.
func detectNode(pkgPath, repoPath string, sig *Signature) {
	b, err := os.ReadFile(pkgPath)
	if err != nil {
		return
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return
	}
	has := func(dep string) bool {
		if _, ok := pkg.Dependencies[dep]; ok {
			return true
		}
		_, ok := pkg.DevDependencies[dep]
		return ok
	}

	if has("react") {
		sig.Frameworks = append(sig.Frameworks, "react")
	}
	if has("vue") {
		sig.Frameworks = append(sig.Frameworks, "vue")
	}
	if has("svelte") {
		sig.Frameworks = append(sig.Frameworks, "svelte")
	}
	if has("next") {
		sig.Frameworks = append(sig.Frameworks, "next")
	}
	if has("@angular/core") {
		sig.Frameworks = append(sig.Frameworks, "angular")
	}
	if has("express") || has("fastify") || has("koa") {
		sig.Frameworks = append(sig.Frameworks, "node-backend")
	}

	if has("vite") ||
		exists(filepath.Join(repoPath, "vite.config.ts")) ||
		exists(filepath.Join(repoPath, "vite.config.js")) {
		sig.BuildTools = append(sig.BuildTools, "vite")
	}
	if has("typescript") {
		sig.Languages = append(sig.Languages, "typescript")
	}

	if has("@playwright/test") || has("playwright") {
		sig.TestTools = append(sig.TestTools, "playwright", "e2e")
	}
	if has("cypress") {
		sig.TestTools = append(sig.TestTools, "cypress", "e2e")
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// DedupeSorted returns xs with empties and duplicates removed, sorted.
func DedupeSorted(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(xs))
	var out []string
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// Union returns the sorted, deduped union of a and b.
func Union(a, b []string) []string {
	return DedupeSorted(append(append([]string{}, a...), b...))
}
