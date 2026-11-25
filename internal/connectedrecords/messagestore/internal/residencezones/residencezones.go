// Package residencezones says which time zone the user lived in on which
// day, so an importer files a message under the day it was where the user was.
package residencezones

import (
	"fmt"
	"strings"
	"time"
)

// Zones says which time zone the owner lived in from which day: a message is
// filed under the day it was where the owner was.
type Zones struct {
	first   *time.Location
	changes []zoneChange
}

type zoneChange struct {
	from     time.Time
	location *time.Location
}

// Parse reads "Europe/Lisbon,2021-07-11=America/Chicago": the
// first zone, then each move as day=zone, in order. A move takes effect at
// midnight UTC of its day.
func Parse(spec string) (Zones, error) {
	parts := strings.Split(spec, ",")
	first, err := time.LoadLocation(strings.TrimSpace(parts[0]))
	if err != nil || strings.TrimSpace(parts[0]) == "" {
		return Zones{}, fmt.Errorf("first zone %q: %v", parts[0], err)
	}
	zones := Zones{first: first}
	for _, part := range parts[1:] {
		day, name, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return Zones{}, fmt.Errorf("%q is not day=zone", part)
		}
		from, err := time.Parse(time.DateOnly, day)
		if err != nil {
			return Zones{}, fmt.Errorf("%q: %w", part, err)
		}
		location, err := time.LoadLocation(name)
		if err != nil {
			return Zones{}, fmt.Errorf("%q: %w", part, err)
		}
		if count := len(zones.changes); count > 0 && !from.After(zones.changes[count-1].from) {
			return Zones{}, fmt.Errorf("%q is not after the move before it", part)
		}
		zones.changes = append(zones.changes, zoneChange{from: from, location: location})
	}
	return zones, nil
}

// At is the zone the owner was in at the moment.
func (zones Zones) At(moment time.Time) *time.Location {
	location := zones.first
	for _, change := range zones.changes {
		if !moment.Before(change.from) {
			location = change.location
		}
	}
	return location
}
