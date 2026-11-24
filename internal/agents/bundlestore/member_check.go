package bundlestore

import "fmt"

// A bundle stores each member as the id skill-store or tool-store assigned,
// with the name beside it only as a label. Either store can delete or rename
// that row at any time, and nothing tells this store. CheckMembers compares
// every stored member with what the two owning stores hold, and reports:
//
//   - a member id the owning store no longer carries. That member resolves to
//     nothing in a session, so the check fails.
//   - a member id the store still carries under a different name than the
//     stored label. The id is what the bundle means, so this is reported and
//     does not fail the check; the label is only out of date.

// MemberEntry is one row an owning store holds, by its id, with its name.
type MemberEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// MemberCheckReport is the outcome of checking stored bundles against the two
// owning stores. Missing lines fail the check; Relabeled lines do not.
type MemberCheckReport struct {
	Missing   []string
	Relabeled []string
}

// Failed reports whether any member names an id its owning store no longer
// carries.
func (report MemberCheckReport) Failed() bool { return len(report.Missing) > 0 }

// CheckMembers checks every member of every given bundle against skills (every
// row skill-store holds) and tools (every row tool-store holds), whatever the
// member's effect or the bundle's enabled flag: a denied tool whose id the
// store no longer carries denies nothing, and a disabled bundle can be enabled
// again tomorrow. It reports every problem rather than stopping at the first.
// A skill and a tool may share an id number; each is looked up only in its own
// store's list.
func CheckMembers(bundles []Bundle, skills, tools []MemberEntry) MemberCheckReport {
	byKind := map[MemberKind]map[int64]MemberEntry{
		MemberSkill: indexMemberEntriesByID(skills),
		MemberTool:  indexMemberEntriesByID(tools),
	}
	var report MemberCheckReport
	for _, b := range bundles {
		for _, m := range b.Members {
			effect := m.Effect
			if effect == "" {
				effect = MemberInclude
			}
			entry, carried := byKind[m.Kind][m.ID]
			if !carried {
				report.Missing = append(report.Missing, fmt.Sprintf(
					"bundle %s (id %d): %s %s id %d, stored as %q: %s has no %s with that id",
					b.Name, b.ID, effect, m.Kind, m.ID, m.Name, m.Kind.OwningStore(), m.Kind))
				continue
			}
			if entry.Name != m.Name {
				report.Relabeled = append(report.Relabeled, fmt.Sprintf(
					"bundle %s (id %d): %s %s id %d is stored as %q, but %s now names it %q",
					b.Name, b.ID, effect, m.Kind, m.ID, m.Name, m.Kind.OwningStore(), entry.Name))
			}
		}
	}
	return report
}

func indexMemberEntriesByID(entries []MemberEntry) map[int64]MemberEntry {
	out := make(map[int64]MemberEntry, len(entries))
	for _, e := range entries {
		out[e.ID] = e
	}
	return out
}
