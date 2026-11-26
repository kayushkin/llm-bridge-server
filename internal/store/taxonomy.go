package store

import "time"

// SessionClassCount is one distinct (type, purpose, origin) triple and how
// many sessions carry it.
//
// The guard works on triples rather than sessions because that is the shape of
// the problem: a purpose typo produces one triple with a count of 1 next to a
// correct triple with a count of thousands, and the useful report names the
// triple, not the four thousand rows that are fine.
type SessionClassCount struct {
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
	Origin  string `json:"origin"`
	Count   int    `json:"count"`
	// FirstSeen and LastSeen bound when sessions with this triple were
	// created. A bad triple whose LastSeen is months old is history that
	// wants a backfill; one still arriving today is a live caller that wants
	// a code change. The report must let a reader tell those apart.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// SessionClassCounts returns every distinct (type, purpose, origin) triple in
// the sessions table with its count and date range, most common first.
func (s *Store) SessionClassCounts() ([]SessionClassCount, error) {
	rows, err := s.dbRO.Query(`
		SELECT COALESCE(type, ''), COALESCE(purpose, ''), COALESCE(origin, ''),
		       COUNT(*), MIN(created_at), MAX(created_at)
		FROM sessions
		GROUP BY 1, 2, 3
		ORDER BY 4 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionClassCount
	for rows.Next() {
		var c SessionClassCount
		if err := rows.Scan(&c.Type, &c.Purpose, &c.Origin, &c.Count, &c.FirstSeen, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
