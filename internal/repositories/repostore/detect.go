package repostore

import "github.com/kayushkin/repodetect"

// Signature and Detect are re-exported from the repodetect module — the single
// source of the detection ruleset — so the store/server keep a stable local
// surface while lean consumers (inber, the resolver) import repodetect directly
// without the SQLite store.
type Signature = repodetect.Signature

// Detect inspects a repo path and returns its signature. See repodetect.Detect.
func Detect(path string) (Signature, error) { return repodetect.Detect(path) }

// unionSorted is the store-side tag union (detected ∪ overrides).
func unionSorted(a, b []string) []string { return repodetect.Union(a, b) }
