package server

import (
	"net/http"
	"sort"

	"github.com/kayushkin/llm-bridge/msg"
)

// The session taxonomy endpoints: what classifications exist, and which
// sessions disagree with them.
//
// Both live here rather than in the guard script that consumes them. The
// vocabulary is defined in Go, in llm-bridge/msg, and a bash script that
// re-listed the valid purposes would be a second copy of it — which is the
// exact failure being fixed, since the folder map was a second copy of the
// same list and drifted from it for months. The guard fetches; it does not
// know what a purpose is.

// taxonomyPurpose is one registry entry on the wire.
type taxonomyPurpose struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Origins      []string `json:"origins,omitempty"`
	Folder       string   `json:"folder,omitempty"`
	Summary      string   `json:"summary"`
	SupersededBy string   `json:"superseded_by,omitempty"`
	// Sessions is how many sessions currently carry this purpose. Zero means
	// a registered purpose nothing creates — either a caller that was removed
	// or an entry added on speculation. Both are worth reporting: a registry
	// with dead entries stops being a description of the system.
	Sessions int `json:"sessions"`
}

// taxonomyFinding is one triple that disagrees with the registry.
type taxonomyFinding struct {
	Type      string               `json:"type"`
	Purpose   string               `json:"purpose"`
	Origin    string               `json:"origin"`
	Sessions  int                  `json:"sessions"`
	FirstSeen string               `json:"first_seen"`
	LastSeen  string               `json:"last_seen"`
	Problems  []msg.PurposeProblem `json:"problems"`
}

// handleSessionTaxonomy returns the classification vocabulary: every valid
// session type, and every registered purpose with a live session count.
func (s *Server) handleSessionTaxonomy(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.SessionClassCounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byPurpose := map[string]int{}
	for _, c := range counts {
		byPurpose[c.Purpose] += c.Count
	}

	types := make([]string, 0, len(msg.SessionTypes()))
	for _, t := range msg.SessionTypes() {
		types = append(types, string(t))
	}

	purposes := make([]taxonomyPurpose, 0)
	for _, p := range msg.KnownPurposes() {
		purposes = append(purposes, taxonomyPurpose{
			Name:         p.Name,
			Type:         string(p.Type),
			Origins:      p.Origins,
			Folder:       p.Folder,
			Summary:      p.Summary,
			SupersededBy: p.SupersededBy,
			Sessions:     byPurpose[p.Name],
		})
	}

	writeJSON(w, map[string]any{
		"types":    types,
		"purposes": purposes,
	})
}

// handleSessionTaxonomyReport returns every (type, purpose, origin) triple in
// the sessions table that disagrees with the registry, plus the counts a
// reader needs to judge severity.
//
// It reports and does not repair. A triple here is a question — is this a
// caller that needs fixing, a purpose the registry should learn, or history to
// backfill? — and answering it wrong automatically is how the original
// mislabelling happened: a migration decided that "no type" meant "interactive"
// and rewrote four thousand rows on that guess.
func (s *Server) handleSessionTaxonomyReport(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.SessionClassCounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var (
		findings      = make([]taxonomyFinding, 0)
		sessionsTotal int
		sessionsBad   int
		byKind        = map[string]int{}
	)

	for _, c := range counts {
		sessionsTotal += c.Count

		var problems []msg.PurposeProblem
		if !msg.ValidSessionType(msg.SessionType(c.Type)) {
			detail := "type is not one this bridge knows how to gate, file or render"
			if c.Type == "" {
				detail = "session has no type, so nothing downstream can tell how it runs"
			}
			problems = append(problems, msg.PurposeProblem{
				Kind: "invalid-type", Detail: detail, Got: c.Type,
			})
		}
		if c.Origin == "" {
			problems = append(problems, msg.PurposeProblem{
				Kind:   "missing-origin",
				Detail: "no record of which service created these sessions",
			})
		}
		problems = append(problems, msg.ClassifyPurpose(msg.SessionType(c.Type), c.Purpose, c.Origin)...)

		if len(problems) == 0 {
			continue
		}
		sessionsBad += c.Count
		for _, p := range problems {
			byKind[p.Kind] += c.Count
		}
		findings = append(findings, taxonomyFinding{
			Type:      c.Type,
			Purpose:   c.Purpose,
			Origin:    c.Origin,
			Sessions:  c.Count,
			FirstSeen: c.FirstSeen.UTC().Format("2006-01-02T15:04:05Z07:00"),
			LastSeen:  c.LastSeen.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Problems:  problems,
		})
	}

	// Registered purposes nothing creates. Reported separately from findings
	// because the fix is the opposite direction: delete the entry, or find out
	// why the caller stopped.
	live := map[string]bool{}
	for _, c := range counts {
		if c.Count > 0 {
			live[c.Purpose] = true
		}
	}
	unused := make([]string, 0)
	for _, p := range msg.KnownPurposes() {
		if !live[p.Name] {
			unused = append(unused, p.Name)
		}
	}
	sort.Strings(unused)

	// Worst first: the triple with the most sessions behind it.
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Sessions > findings[j].Sessions })

	writeJSON(w, map[string]any{
		"sessions_total":   sessionsTotal,
		"sessions_flagged": sessionsBad,
		"triples_total":    len(counts),
		"triples_flagged":  len(findings),
		"problems_by_kind": byKind,
		"unused_purposes":  unused,
		"findings":         findings,
	})
}
