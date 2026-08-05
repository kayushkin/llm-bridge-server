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
	//
	// Zero when the stored timestamp could not be parsed. created_at holds a
	// mix of formats on this box — rows written before the driver was pinned
	// to _time_format=sqlite carry Go's time.Time.String() output instead of
	// a SQLite datetime — and MIN/MAX strips the column's type affinity, so
	// the driver hands back a bare string either way. An unparseable date is
	// not worth failing the whole report over; the counts are the point.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// sessionTimeLayouts are the formats created_at is known to hold, newest
// convention first.
var sessionTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	time.RFC3339Nano,
	"2006-01-02 15:04:05",
}

// parseSessionTime reads a stored created_at, returning the zero time if none
// of the known layouts match.
func parseSessionTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range sessionTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
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
		var first, last string
		if err := rows.Scan(&c.Type, &c.Purpose, &c.Origin, &c.Count, &first, &last); err != nil {
			return nil, err
		}
		c.FirstSeen = parseSessionTime(first)
		c.LastSeen = parseSessionTime(last)
		out = append(out, c)
	}
	return out, rows.Err()
}
