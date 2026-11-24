package bundlestore

import (
	"errors"
	"fmt"
)

// ErrBundleNotApplicable is returned when an explicitly requested bundle id
// cannot be applied — it does not exist or is disabled. The handler maps it
// to 404 so a caller can tell "your id is wrong" from a resolver fault.
var ErrBundleNotApplicable = errors.New("bundle not applicable")

// ResolveRequest carries the signals the resolver decides a session bundle
// from: the target repo's effective tags (from repo-store) and the classified
// task intent tags.
type ResolveRequest struct {
	RepoTags []string `json:"repo_tags"`
	TaskTags []string `json:"task_tags,omitempty"`
	// BundleIDs, when given, replaces tag selection: exactly these bundles
	// (by bundle-store id) are applied, each expanded through its extends
	// chain, plus "base" as always. This is how a session started with an
	// explicit bundle — a kanban board's default, a caller's choice — is
	// composed. An id that does not exist or names a disabled bundle is a
	// loud error, never silently dropped: a session that asked for a bundle
	// and got a smaller one looks perfectly valid and is not what was asked.
	BundleIDs []int64 `json:"bundle_ids,omitempty"`
}

// ResolveResponse is the composed bundle: the skill-store skills to
// ensure-installed, the tool-store tools to provision, and optional
// model/effort defaults. Each carries the owning store's id with its name for
// display — a consumer installs a skill with POST /skills/{id}/install and asks
// tool-store for a tool by id, and never searches either store by name.
// Bundles lists which definitions contributed (after inheritance), for
// UI/debugging.
//
// DeniedTools are the tools the session must not have: every tool a bundle in
// the resolved set denies, including tools a harness turns on by default. A
// deny anywhere wins — a tool one bundle denies is left out of Tools even when
// another bundle includes it. DeniedReadPaths are the path patterns the
// session may not read, the union across the resolved set in the order first
// seen. Both are always present, empty when nothing is denied.
type ResolveResponse struct {
	Bundles         []BundleRef `json:"bundles"`
	Skills          []MemberRef `json:"skills"`
	Tools           []MemberRef `json:"tools"`
	DeniedTools     []MemberRef `json:"denied_tools"`
	DeniedReadPaths []string    `json:"denied_read_paths"`
	Model           string      `json:"model,omitempty"`
	Effort          string      `json:"effort,omitempty"`
}

// Resolve composes the bundle for a request. It is a pure join over the curated
// definitions — it returns the member ids the curator recorded, never reaching
// into skill-store or tool-store. Two bundles contributing the same member
// collapse to one entry because they name the same id; two skills that merely
// share a name do not. Selection: every enabled bundle named "base" (universal) or whose
// match_tags intersect the repo tags. Each selected bundle is expanded through
// its Extends chain (ancestor-first, cycle-guarded). Members with a Condition
// are included only when the task tags contain that condition. Model/Effort:
// last non-empty wins across the applied bundles (keep them on one base bundle
// for deterministic results).
//
// Denies: the resolved set is every selected bundle and every ancestor its
// extends chain reaches. A tool denied by any of them is removed from Tools
// and listed in DeniedTools, whichever bundle included it and in whatever
// order the bundles were applied. A conditional deny applies only when its
// condition is among the task tags, exactly like a conditional include.
// Denied read paths from every bundle in the set are unioned.
func Resolve(s *Store, req ResolveRequest) (*ResolveResponse, error) {
	all, err := s.ListBundles(ListFilter{EnabledOnly: true})
	if err != nil {
		return nil, err
	}
	repoSet := toSet(req.RepoTags)
	taskSet := toSet(req.TaskTags)

	var selected []Bundle
	if len(req.BundleIDs) > 0 {
		byID := make(map[int64]Bundle, len(all))
		for _, b := range all {
			byID[b.ID] = b
		}
		for _, b := range all {
			if b.Name == "base" {
				selected = append(selected, b)
			}
		}
		for _, id := range req.BundleIDs {
			b, ok := byID[id]
			if !ok {
				if _, err := s.GetBundle(id); err == nil {
					return nil, fmt.Errorf("%w: bundle id %d is disabled and cannot be applied to a session", ErrBundleNotApplicable, id)
				}
				return nil, fmt.Errorf("%w: bundle id %d does not exist", ErrBundleNotApplicable, id)
			}
			selected = append(selected, b)
		}
	} else {
		for _, b := range all {
			if b.Name == "base" || intersects(b.MatchTags, repoSet) {
				selected = append(selected, b)
			}
		}
	}

	resp := &ResolveResponse{}
	usedBundles := newOrderedBundleSet()
	skills := newOrderedMemberSet()
	tools := newOrderedMemberSet()
	deniedTools := newOrderedMemberSet()
	deniedReadPaths := newOrderedStringSet()

	for i := range selected {
		chain, err := expandChain(s, selected[i])
		if err != nil {
			return nil, err
		}
		for _, cb := range chain {
			usedBundles.add(BundleRef{ID: cb.ID, Name: cb.Name})
			if cb.Model != "" {
				resp.Model = cb.Model
			}
			if cb.Effort != "" {
				resp.Effort = cb.Effort
			}
			for _, p := range cb.DeniedReadPaths {
				deniedReadPaths.add(p)
			}
			for _, m := range cb.Members {
				if m.Condition != "" && !taskSet[m.Condition] {
					continue
				}
				if m.Effect == MemberDeny {
					if m.Kind != MemberTool {
						return nil, fmt.Errorf("resolve: bundle %q denies %s member id %d, and only a tool can be denied", cb.Name, m.Kind, m.ID)
					}
					deniedTools.add(MemberRef{ID: m.ID, Name: m.Name})
					continue
				}
				if m.Effect != MemberInclude {
					return nil, fmt.Errorf("resolve: bundle %q has %s member id %d with unknown effect %q", cb.Name, m.Kind, m.ID, m.Effect)
				}
				switch m.Kind {
				case MemberSkill:
					skills.add(MemberRef{ID: m.ID, Name: m.Name})
				case MemberTool:
					tools.add(MemberRef{ID: m.ID, Name: m.Name})
				}
			}
		}
	}

	resp.Bundles = usedBundles.list()
	resp.Skills = skills.list()
	resp.Tools = tools.without(deniedTools.seen)
	resp.DeniedTools = deniedTools.list()
	resp.DeniedReadPaths = deniedReadPaths.list()
	return resp, nil
}

// expandChain returns b's ancestor bundles first, then b itself, walking the
// ExtendsID links. Every hop is a lookup by the parent's own id; the parent's
// name is carried along for the error messages and is never what finds the row.
// A missing parent or a cycle is a loud error — no silent truncation of the
// chain, because a truncated chain resolves to a smaller bundle that looks
// perfectly valid.
//
// UpsertBundle refuses both of those at the write, so neither should be
// reachable through the API. This guard is for the rows that did not come
// through it: a database written by hand, or one that predates that check.
func expandChain(s *Store, b Bundle) ([]Bundle, error) {
	chain := []Bundle{b}
	seen := map[int64]bool{b.ID: true}
	child := b
	for child.ExtendsID != 0 {
		parent, err := s.GetBundle(child.ExtendsID)
		if err != nil {
			return nil, fmt.Errorf("resolve: bundle %q extends bundle id %d, which is missing: %w", child.Name, child.ExtendsID, err)
		}
		if seen[parent.ID] {
			return nil, fmt.Errorf("resolve: inheritance cycle — bundle %q extends %q, which already inherits from it", child.Name, parent.Name)
		}
		seen[parent.ID] = true
		// prepend so ancestors come first
		chain = append([]Bundle{*parent}, chain...)
		child = *parent
	}
	return chain, nil
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func intersects(tags []string, set map[string]bool) bool {
	for _, t := range tags {
		if set[t] {
			return true
		}
	}
	return false
}

// orderedMemberSet preserves first-insertion order while deduping on the
// owning store's id. Deduping on the name instead would silently merge two
// different skills that happen to share one.
type orderedMemberSet struct {
	seen  map[int64]bool
	items []MemberRef
}

func newOrderedMemberSet() *orderedMemberSet {
	return &orderedMemberSet{seen: map[int64]bool{}}
}

func (o *orderedMemberSet) add(m MemberRef) {
	if m.ID <= 0 || o.seen[m.ID] {
		return
	}
	o.seen[m.ID] = true
	o.items = append(o.items, m)
}

func (o *orderedMemberSet) list() []MemberRef {
	if o.items == nil {
		return []MemberRef{}
	}
	return o.items
}

// without lists the members whose id is not in excluded, keeping order.
func (o *orderedMemberSet) without(excluded map[int64]bool) []MemberRef {
	out := []MemberRef{}
	for _, m := range o.items {
		if !excluded[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// orderedStringSet keeps first-insertion order and drops repeats.
type orderedStringSet struct {
	seen  map[string]bool
	items []string
}

func newOrderedStringSet() *orderedStringSet {
	return &orderedStringSet{seen: map[string]bool{}}
}

func (o *orderedStringSet) add(s string) {
	if o.seen[s] {
		return
	}
	o.seen[s] = true
	o.items = append(o.items, s)
}

func (o *orderedStringSet) list() []string {
	if o.items == nil {
		return []string{}
	}
	return o.items
}

// orderedBundleSet is the same, over bundle-store's own ids.
type orderedBundleSet struct {
	seen  map[int64]bool
	items []BundleRef
}

func newOrderedBundleSet() *orderedBundleSet {
	return &orderedBundleSet{seen: map[int64]bool{}}
}

func (o *orderedBundleSet) add(b BundleRef) {
	if b.ID <= 0 || o.seen[b.ID] {
		return
	}
	o.seen[b.ID] = true
	o.items = append(o.items, b)
}

func (o *orderedBundleSet) list() []BundleRef {
	if o.items == nil {
		return []BundleRef{}
	}
	return o.items
}
