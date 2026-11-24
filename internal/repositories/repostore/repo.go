// Package repostore is the canonical registry of repositories and their
// detected signature (language/framework/build/test tags). It answers the
// stable question "what IS this repo", independent of any agent session or
// materialized worktree. Repo tags feed the bundle resolver, which selects the
// skills/tools/MCPs to provision for a session.
package repostore

import "sort"

// Repo is a registry entry for one repository. Tags are computed by Detect
// from the repo's manifests; TagOverrides are manual additions a human curates
// when detection misses something. EffectiveTags (the union) is what callers
// match against — it is derived on read, never stored.
type Repo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	GitRemote string `json:"git_remote,omitempty"`
	// GitHubURL is the repository's page on GitHub
	// (https://github.com/<owner>/<name>), derived from GitRemote on read;
	// empty when the remote is not on GitHub. See GitHubURLOfRemote.
	GitHubURL   string `json:"github_url,omitempty"`
	Description string `json:"description,omitempty"`

	// Provenance is who authors this repository: "ours", "vendored", or
	// empty when nobody has stated it. It is not detected — see Provenance.
	Provenance Provenance `json:"provenance"`

	Tags         []string `json:"tags"`                    // detected signature
	TagOverrides []string `json:"tag_overrides,omitempty"` // manual additive tags
	// EffectiveTags = union(Tags, TagOverrides), sorted+deduped. Derived on
	// read; there is no column for it.
	EffectiveTags []string `json:"effective_tags"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Provenance says who authors a repository. It exists because every
// fleet-wide count taken over the host — tests, functions, flagged rows —
// sums over a repository set, and a set that silently includes vendored
// upstream code overstates the work in code anyone here may actually repair.
// A number whose population is not stated is not a measurement, and this is
// the field that lets the population be stated.
//
// The store records what it was told and never infers a value. An
// unstated provenance therefore stays unstated rather than defaulting to
// ProvenanceOurs: defaulting would put a guess underneath every number taken
// over the registry, in the direction that makes the backlog look bigger.
type Provenance string

const (
	// ProvenanceUnstated is a repository nobody has classified yet. It is the
	// zero value, and a caller that needs the answer must treat it as missing
	// rather than as either of the other two.
	ProvenanceUnstated Provenance = ""
	// ProvenanceOurs is a repository authored and maintained on the host.
	ProvenanceOurs Provenance = "ours"
	// ProvenanceVendored is third-party code kept here for reference or use —
	// an upstream checkout, a vendored dependency, demo samples. Changing it
	// is changing somebody else's project, so it does not belong in a count of
	// work the host can do.
	ProvenanceVendored Provenance = "vendored"
)

// knownProvenances is the closed set the API validates against. It is a set
// rather than a chain of comparisons so that adding a value is one line and
// cannot be added to one check and forgotten in another.
var knownProvenances = map[Provenance]struct{}{
	ProvenanceUnstated: {},
	ProvenanceOurs:     {},
	ProvenanceVendored: {},
}

// IsKnown reports whether p is one of the values this store accepts.
func (p Provenance) IsKnown() bool {
	_, ok := knownProvenances[p]
	return ok
}

// KnownProvenances returns the accepted values, sorted, for error messages and
// for any caller that wants to render the choice rather than restate it.
func KnownProvenances() []Provenance {
	out := make([]Provenance, 0, len(knownProvenances))
	for p := range knownProvenances {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ProvenanceUnstatedQueryValue is the word a query uses to ask for the
// repositories whose provenance is unstated. An empty query parameter cannot
// mean it: "?provenance=" is indistinguishable from the parameter being absent,
// which is the filter being off.
const ProvenanceUnstatedQueryValue = "unstated"
