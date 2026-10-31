package principalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Availability is a human's declared working week, in their own zone. It is
// the same shape as kanban-store's board BusinessHours on purpose, so the two
// can be read by the same code, but it belongs to the person: a board in
// Los Angeles can have a member in Amsterdam.
//
// A principal with no Availability is *unknown*, and unknown is never
// available. There is no default zone — a guessed zone would make a person
// look free at 3am and an assignment made on that guess is worse than none.
type Availability struct {
	TZID  string   `json:"tzid"`
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
}

// WeekdayCodes is the RFC 5545 vocabulary for Days, served at
// GET /weekday-codes so no caller hardcodes it.
var WeekdayCodes = []string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"}

var weekdayByCode = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// Validate checks the week is one a clock can evaluate: a zone that loads,
// at least one known day, and HH:MM bounds with start before end. Day codes
// are upper-cased and de-duplicated in place.
func (a *Availability) Validate() error {
	a.TZID = strings.TrimSpace(a.TZID)
	if a.TZID == "" {
		return fmt.Errorf("%w: availability.tzid is required — an offset is not a zone, and without one the hours drift at every DST change", ErrInvalidAvailability)
	}
	if _, err := time.LoadLocation(a.TZID); err != nil {
		return fmt.Errorf("%w: availability.tzid %q is not a zone this host knows: %v", ErrInvalidAvailability, a.TZID, err)
	}
	if len(a.Days) == 0 {
		return fmt.Errorf("%w: availability.days must name at least one of %s", ErrInvalidAvailability, strings.Join(WeekdayCodes, ", "))
	}
	seen := map[string]bool{}
	days := make([]string, 0, len(a.Days))
	for _, raw := range a.Days {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if _, ok := weekdayByCode[code]; !ok {
			return fmt.Errorf("%w: availability.days has unknown day %q: use %s", ErrInvalidAvailability, raw, strings.Join(WeekdayCodes, ", "))
		}
		if !seen[code] {
			seen[code] = true
			days = append(days, code)
		}
	}
	a.Days = days
	start, err := parseClockTime(a.Start)
	if err != nil {
		return fmt.Errorf("%w: availability.start: %v", ErrInvalidAvailability, err)
	}
	end, err := parseClockTime(a.End)
	if err != nil {
		return fmt.Errorf("%w: availability.end: %v", ErrInvalidAvailability, err)
	}
	if start >= end {
		return fmt.Errorf("%w: availability.start %s must be before availability.end %s", ErrInvalidAvailability, a.Start, a.End)
	}
	return nil
}

// parseClockTime reads "HH:MM" as minutes past midnight.
func parseClockTime(raw string) (int, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", raw)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, fmt.Errorf("%q is not HH:MM: hour out of range", raw)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("%q is not HH:MM: minute out of range", raw)
	}
	return hour*60 + minute, nil
}

// inHours reports whether the instant falls inside the working week, read in
// the availability's own zone. Validate must have passed.
func (a *Availability) inHours(at time.Time) bool {
	location, err := time.LoadLocation(a.TZID)
	if err != nil {
		return false
	}
	local := at.In(location)
	dayMatches := false
	for _, code := range a.Days {
		if weekdayByCode[code] == local.Weekday() {
			dayMatches = true
			break
		}
	}
	if !dayMatches {
		return false
	}
	start, _ := parseClockTime(a.Start)
	end, _ := parseClockTime(a.End)
	minutes := local.Hour()*60 + local.Minute()
	return minutes >= start && minutes < end
}

// The reason vocabulary is served (GET /availability-reasons) so a caller
// that branches on it never guesses the spelling. Exactly one reason applies
// at any instant, checked in this order: a disabled principal is never
// available whatever its hours; a principal with no schedule is unknown;
// time off overrides the week; then the week itself decides.
const (
	ReasonInHours    = "in_hours"
	ReasonOffHours   = "off_hours"
	ReasonTimeOff    = "time_off"
	ReasonNoSchedule = "no_schedule"
	ReasonDisabled   = "disabled"
)

var AvailabilityReasons = []string{ReasonInHours, ReasonOffHours, ReasonTimeOff, ReasonNoSchedule, ReasonDisabled}

// AvailabilityAnswer is what GET /principals/{id}/availability returns.
// Available is true for exactly one reason, in_hours; the others say why not.
type AvailabilityAnswer struct {
	PrincipalID string `json:"principal_id"`
	At          int64  `json:"at"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason"`
}

// TimeOff is one absence: a half-open range [starts_at, ends_at) in epoch
// seconds. It sits beside the week rather than editing it, so a holiday does
// not mean rewriting the schedule and restoring it after.
type TimeOff struct {
	ID          string `json:"id"`
	Seq         int64  `json:"seq"`
	PrincipalID string `json:"principal_id"`
	StartsAt    int64  `json:"starts_at"`
	EndsAt      int64  `json:"ends_at"`
	Note        string `json:"note"`
	CreatedAt   int64  `json:"created_at"`
}

var ErrInvalidAvailability = errors.New("invalid availability")

func formatTimeOffID(seq int64) string { return fmt.Sprintf("timeoff_%06d", seq) }

// evaluate decides one principal's availability at an instant, given the
// absences that overlap it. onTimeOff is whether any time_off row covers at.
func evaluate(p *Principal, at time.Time, onTimeOff bool) AvailabilityAnswer {
	answer := AvailabilityAnswer{PrincipalID: p.ID, At: at.Unix()}
	switch {
	case p.DisabledAt != 0:
		answer.Reason = ReasonDisabled
	case p.Availability == nil:
		answer.Reason = ReasonNoSchedule
	case onTimeOff:
		answer.Reason = ReasonTimeOff
	case p.Availability.inHours(at):
		answer.Reason = ReasonInHours
		answer.Available = true
	default:
		answer.Reason = ReasonOffHours
	}
	return answer
}

// Availability answers whether one principal is available at an instant.
// A group has no hours of its own — ask its members.
func (s *Store) Availability(id string, at time.Time) (AvailabilityAnswer, error) {
	p, err := s.getRow(id)
	if err != nil {
		return AvailabilityAnswer{}, err
	}
	if p.Kind == KindGroup {
		return AvailabilityAnswer{}, fmt.Errorf("%w: %s is a group and has no hours of its own — read GET /principals/%s/members?available_at=%d", ErrInvalidAvailability, id, id, at.Unix())
	}
	off, err := s.principalsOnTimeOff(at, []string{id})
	if err != nil {
		return AvailabilityAnswer{}, err
	}
	return evaluate(p, at, off[id]), nil
}

// principalsOnTimeOff returns which of the given principals have an absence
// covering the instant, in one query.
func (s *Store) principalsOnTimeOff(at time.Time, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+2)
	args = append(args, at.Unix(), at.Unix())
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT DISTINCT principal_id FROM time_off
		WHERE starts_at <= ? AND ends_at > ? AND principal_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// filterAvailable keeps the principals available at the instant. The caller
// has already restricted the list to humans.
func (s *Store) filterAvailable(principals []*Principal, at time.Time) ([]*Principal, error) {
	ids := make([]string, 0, len(principals))
	for _, p := range principals {
		ids = append(ids, p.ID)
	}
	off, err := s.principalsOnTimeOff(at, ids)
	if err != nil {
		return nil, err
	}
	out := []*Principal{}
	for _, p := range principals {
		if evaluate(p, at, off[p.ID]).Available {
			out = append(out, p)
		}
	}
	return out, nil
}

// setAvailability replaces a human's week, or clears it when availability is
// nil. A group is refused: it has no hours of its own.
func (s *Store) setAvailability(p *Principal, availability *Availability) error {
	if availability == nil {
		p.Availability = nil
		return nil
	}
	if p.Kind == KindGroup {
		return fmt.Errorf("%w: %s is a group — availability belongs to its members, and a group's is derived from theirs", ErrInvalidAvailability, p.ID)
	}
	if p.Kind == KindPeer {
		return fmt.Errorf("%w: %s is a peer — another server has no working week; availability belongs to a human", ErrInvalidAvailability, p.ID)
	}
	if p.Kind == KindService {
		return fmt.Errorf("%w: %s is a service — a program has no working week; availability belongs to a human", ErrInvalidAvailability, p.ID)
	}
	if err := availability.Validate(); err != nil {
		return err
	}
	p.Availability = availability
	return nil
}

func encodeAvailability(a *Availability) (string, error) {
	if a == nil {
		return "", nil
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeAvailability(raw string) (*Availability, error) {
	if raw == "" {
		return nil, nil
	}
	var a Availability
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("unreadable availability %q: %w", raw, err)
	}
	return &a, nil
}

// AddTimeOff records an absence for a human. The range must be non-empty;
// overlapping ranges are allowed, since two reasons to be away on one day
// are still one day away.
func (s *Store) AddTimeOff(principalID string, startsAt, endsAt int64, note string) (*TimeOff, error) {
	if _, err := s.requireKind(principalID, KindHuman, "time off belongs to a human"); err != nil {
		if errors.Is(err, ErrInvalidMembership) {
			return nil, fmt.Errorf("%w: %s is a group — time off belongs to a human", ErrInvalidAvailability, principalID)
		}
		return nil, err
	}
	if startsAt <= 0 || endsAt <= 0 {
		return nil, fmt.Errorf("%w: starts_at and ends_at are required, as epoch seconds", ErrInvalidAvailability)
	}
	if startsAt >= endsAt {
		return nil, fmt.Errorf("%w: starts_at %d must be before ends_at %d", ErrInvalidAvailability, startsAt, endsAt)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM time_off`).Scan(&maxSeq); err != nil {
		return nil, err
	}
	row := &TimeOff{
		Seq: maxSeq.Int64 + 1, PrincipalID: principalID, StartsAt: startsAt, EndsAt: endsAt,
		Note: strings.TrimSpace(note), CreatedAt: now(),
	}
	row.ID = formatTimeOffID(row.Seq)
	if _, err := tx.Exec(`INSERT INTO time_off (id, seq, principal_id, starts_at, ends_at, note, created_at) VALUES (?,?,?,?,?,?,?)`,
		row.ID, row.Seq, row.PrincipalID, row.StartsAt, row.EndsAt, row.Note, row.CreatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return row, nil
}

// ListTimeOff returns a principal's absences, soonest first. Disabled rows
// answer like any other; a group is a 400 because it has none.
func (s *Store) ListTimeOff(principalID string) ([]*TimeOff, error) {
	p, err := s.getRow(principalID)
	if err != nil {
		return nil, err
	}
	if p.Kind == KindGroup {
		return nil, fmt.Errorf("%w: %s is a group — time off belongs to a human", ErrInvalidAvailability, principalID)
	}
	rows, err := s.db.Query(`SELECT id, seq, principal_id, starts_at, ends_at, note, created_at
		FROM time_off WHERE principal_id = ? ORDER BY starts_at ASC, seq ASC`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TimeOff{}
	for rows.Next() {
		var t TimeOff
		if err := rows.Scan(&t.ID, &t.Seq, &t.PrincipalID, &t.StartsAt, &t.EndsAt, &t.Note, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RemoveTimeOff deletes one absence. This is a real delete — unlike a
// principal, nothing joins on a time_off id — and the pair must match, so a
// row cannot be removed through another principal's path.
func (s *Store) RemoveTimeOff(principalID, timeOffID string) error {
	if _, err := s.getRow(principalID); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM time_off WHERE id = ? AND principal_id = ?`, timeOffID, principalID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: time off %s on principal %s", ErrNotFound, timeOffID, principalID)
	}
	return nil
}
