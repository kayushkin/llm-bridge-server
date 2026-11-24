package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/noteboard/internal/boundedtext"
	"github.com/teambition/rrule-go"
)

// Schedule is the recurrence rule for an item, expressed in RFC 5545 (iCalendar)
// terms. It answers two independent questions that were previously conflated and
// hardcoded into cron jobs:
//
//	when is this DUE      — DTStart + RRule + RDate - ExDate
//	when do I get NUDGED  — Remind.Lead (before due) and Remind.Nag (until done)
//
// A task can have either, both, or neither. "Water the plants every 3rd Tuesday"
// is a due recurrence with no nag. "Call the tag agency" is a one-shot due date
// with a weekday nag until it is done. Those are different axes, so they are
// different fields.
//
// Schedule is the RULE. It is NOT the answer. Item.DueAt remains the single
// source of truth for "when is this next due" — the coordinator expands the rule
// and writes the resulting instant into DueAt (rolling mode) or into a freshly
// materialized child item's DueAt (instance mode). DTStart is the ANCHOR of the
// series, in the iCalendar sense: where the recurrence starts counting from and
// what time of day it lands on. It is not "the next due date" and must never be
// read as one.
//
// RFC 5545 rather than a homemade cadence grammar because it already expresses
// every case anyone asks for (every 3rd Tuesday: FREQ=MONTHLY;BYDAY=3TU; last
// weekday of the month: FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1), and
// because Google Calendar consumes it natively — so mirroring an item to the
// calendar is a pass-through, not a translation that could disagree with us.
type Schedule struct {
	// DTStart anchors the series: the first candidate instant, and the time of
	// day every occurrence inherits. Required whenever RRule is set.
	DTStart *time.Time `json:"dtstart,omitempty"`

	// TZID is the IANA zone the rule is expanded in (e.g. "America/Los_Angeles").
	// Required whenever a rule is present, and deliberately NOT defaulted: an
	// offset is not a zone. A stored offset like -07:00 cannot tell you what
	// happens after a DST transition, so a rule anchored only to an offset
	// silently drifts by an hour twice a year — which is exactly the bug the cron
	// nudges this replaces have today. "9am on the 3rd Tuesday" means 9am local
	// in a named zone or it means nothing.
	TZID string `json:"tzid,omitempty"`

	// RRule is the recurrence, without the "RRULE:" prefix (though one is
	// tolerated): "FREQ=MONTHLY;BYDAY=3TU". Empty means the item is a one-shot.
	RRule string `json:"rrule,omitempty"`

	// RDate adds explicit one-off occurrences on top of RRule. An item with only
	// RDate and no RRule is "these specific dates and no others".
	//
	// Applies to the DUE series only, never to Remind.Nag. Both used to be fed
	// into whichever rule was being expanded, so adding one extra due date also
	// bought an extra nag on that date — a rule the user never wrote.
	RDate []time.Time `json:"rdate,omitempty"`

	// ExDate removes occurrences the rule would otherwise produce (a skipped
	// week). Due series only, on the same grounds as RDate.
	ExDate []time.Time `json:"exdate,omitempty"`

	// Mode decides what an occurrence MEANS. See ScheduleMode* below.
	Mode string `json:"mode,omitempty"`

	Remind *Remind `json:"remind,omitempty"`

	// LastMaterializedAt is the occurrence instant the coordinator has already
	// acted on. It is bookkeeping written by the coordinator, not by the user,
	// and it is what makes materialization idempotent: a coordinator run that
	// crashes and reruns, or a */15 tick that fires twice, must not produce two
	// child items for the same Tuesday.
	LastMaterializedAt *time.Time `json:"last_materialized_at,omitempty"`

	// LastNaggedAt is the last time this item's nag rule actually reached the
	// user. Same purpose, other axis: the coordinator ticks every 15 minutes, so
	// without it a single 10am firing would be re-sent on all 96 ticks of the day.
	LastNaggedAt *time.Time `json:"last_nagged_at,omitempty"`
}

// What an occurrence means when it comes due.
const (
	// ScheduleModeInstance materializes a child item per occurrence (via
	// ParentID). The scheduled item is a TEMPLATE: it never itself appears as
	// work, each occurrence is an ordinary independent todo, and completing one
	// week's does not touch the next. This keeps every consumer downstream dumb —
	// the digest just lists open todos and never needs to understand recurrence.
	ScheduleModeInstance = "instance"

	// ScheduleModeRolling advances the item's own DueAt to the next occurrence
	// when it is completed. One row, no children, no completion history. Right
	// for "renew the registration annually", where last year's is of no interest.
	ScheduleModeRolling = "rolling"
)

// Channels a reminder can be delivered on. The digest is the primary surface and
// is always available; calendar and herald are opt-in per item.
const (
	ChannelDigest   = "digest"
	ChannelCalendar = "calendar"
	ChannelHerald   = "herald"
)

// Remind is how an item gets surfaced. Orthogonal to when it is due.
type Remind struct {
	// Lead fires ahead of a due occurrence: ISO 8601 durations, negative for
	// "before" ("-P1D", "-PT2H"). Calendar-aware — "-P1D" is one calendar day,
	// not 24 hours, so it stays correct across a DST boundary.
	Lead []string `json:"lead,omitempty"`

	// Nag repeats until the item is done, independent of its due date. This is
	// the "keep bothering me" rule: FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=10.
	// It anchors on DTStart, or on the item's DueAt if DTStart is unset.
	Nag string `json:"nag,omitempty"`

	// Channels defaults to digest-only when empty.
	Channels []string `json:"channels,omitempty"`
}

// DeliversOn is nil-safe on purpose. An item carrying a recurrence but no
// `remind` block at all is the ordinary case — a chore that recurs and wants the
// default treatment — and the coordinator asks every scheduled item which
// channels it wants. A nil Remind means "defaults", not "no reminders".
func (r *Remind) DeliversOn(channel string) bool {
	if r == nil || len(r.Channels) == 0 {
		return channel == ChannelDigest
	}
	for _, c := range r.Channels {
		if c == channel {
			return true
		}
	}
	return false
}

// Validate checks the SHAPE of the schedule: that every rule parses, that a zone
// is named whenever one is needed, and that no field is quietly meaningless. It
// runs on write so a malformed rule is rejected at the API with a message the
// caller can act on, rather than discovered months later by a coordinator that
// expands it to nothing and silently reminds no one.
//
// It cannot check the anchor, because on a PATCH the anchor may live on the
// stored item rather than in the request. That check is Anchor()'s job.
func (s *Schedule) Validate() error {
	if s == nil {
		return nil
	}

	hasRule := s.RRule != "" || s.Remind.nagRule() != ""
	if hasRule {
		if s.TZID == "" {
			return fmt.Errorf("schedule.tzid is required when a recurrence rule is set (e.g. \"America/Los_Angeles\") — an offset is not a zone and cannot survive a DST transition")
		}
		if _, err := time.LoadLocation(s.TZID); err != nil {
			return fmt.Errorf("schedule.tzid %q is not a known IANA zone: %w", boundedtext.Value(s.TZID), err)
		}
	}

	if s.RRule != "" {
		if s.DTStart == nil {
			return fmt.Errorf("schedule.dtstart is required when schedule.rrule is set — the rule needs an anchor to count from and a time of day to land on")
		}
		if _, err := parseRRule(s.RRule); err != nil {
			return fmt.Errorf("schedule.rrule is not a valid RFC 5545 rule: %w", err)
		}
	}

	if nag := s.Remind.nagRule(); nag != "" {
		if _, err := parseRRule(nag); err != nil {
			return fmt.Errorf("schedule.remind.nag is not a valid RFC 5545 rule: %w", err)
		}
	}

	switch s.Mode {
	case "", ScheduleModeInstance, ScheduleModeRolling:
	default:
		return fmt.Errorf("schedule.mode must be %q or %q, got %q", ScheduleModeInstance, ScheduleModeRolling, boundedtext.Value(s.Mode))
	}

	if s.Remind != nil {
		for _, lead := range s.Remind.Lead {
			if _, err := ParseISO8601Duration(lead); err != nil {
				return fmt.Errorf("schedule.remind.lead %q: %s", boundedtext.Value(lead), boundedtext.ErrorMessage(err))
			}
		}
		for _, c := range s.Remind.Channels {
			switch c {
			case ChannelDigest, ChannelCalendar, ChannelHerald:
			default:
				return fmt.Errorf("schedule.remind.channels contains unknown channel %q (want %q, %q, or %q)", boundedtext.Value(c), ChannelDigest, ChannelCalendar, ChannelHerald)
			}
		}
	}

	return nil
}

func (r *Remind) nagRule() string {
	if r == nil {
		return ""
	}
	return r.Nag
}

// EffectiveMode is the mode an item with a due recurrence acts under. Instance is
// the default: it gives a completion record and keeps recurrence out of every
// downstream consumer.
func (s *Schedule) EffectiveMode() string {
	if s.Mode == "" {
		return ScheduleModeInstance
	}
	return s.Mode
}

// HasDueRecurrence reports whether the schedule generates due dates at all (as
// opposed to only nagging about a fixed one).
func (s *Schedule) HasDueRecurrence() bool {
	return s != nil && (s.RRule != "" || len(s.RDate) > 0)
}

// Location is the zone the rule expands in.
func (s *Schedule) Location() (*time.Location, error) {
	if s.TZID == "" {
		return nil, fmt.Errorf("schedule has no tzid")
	}
	return time.LoadLocation(s.TZID)
}

// Anchor resolves the instant a rule counts from. DTStart if the caller set one,
// otherwise the item's own due date — a one-shot task with a nag ("call the tag
// agency, bother me every weekday") has a due date and no reason to also restate
// it as a dtstart.
//
// It returns an error rather than inventing an anchor. Falling back to "now"
// would make a rule expand differently on every run, which is the kind of thing
// that looks like it works and silently reminds you on the wrong Tuesday.
func (s *Schedule) Anchor(dueAt *time.Time) (time.Time, error) {
	if s.DTStart != nil {
		return *s.DTStart, nil
	}
	if dueAt != nil {
		return *dueAt, nil
	}
	return time.Time{}, fmt.Errorf("schedule has no anchor: set schedule.dtstart, or give the item a due_at")
}

// parseRRule tolerates the "RRULE:" line prefix so a rule copied straight out of
// an .ics file or a Google Calendar payload works as-is.
func parseRRule(s string) (*rrule.ROption, error) {
	return rrule.StrToROption(strings.TrimPrefix(strings.TrimSpace(s), "RRULE:"))
}

// DueOccurrences expands the due rule into the instants it produces in
// [from, to], applying RDate and ExDate. Results are in the schedule's zone, so
// "9am on the 3rd Tuesday" is 9am local on both sides of a DST change.
func (s *Schedule) DueOccurrences(dueAt *time.Time, from, to time.Time) ([]time.Time, error) {
	if !s.HasDueRecurrence() {
		return nil, nil
	}
	return s.expand(s.RRule, s.RDate, s.ExDate, dueAt, from, to)
}

// NagOccurrences expands the nag rule — the instants an unfinished item should
// bother the user, independent of when it is due.
//
// RDate and ExDate are deliberately NOT passed: they describe the due series,
// and feeding them to the nag rule made one extra due date fire one extra nag.
func (s *Schedule) NagOccurrences(dueAt *time.Time, from, to time.Time) ([]time.Time, error) {
	if s.Remind.nagRule() == "" {
		return nil, nil
	}
	return s.expand(s.Remind.Nag, nil, nil, dueAt, from, to)
}

// expand turns one rule into the instants it produces in [from, to]. The RDATE
// and EXDATE sets are parameters rather than reads off s, so each caller states
// which series the dates belong to instead of every rule silently inheriting
// them.
func (s *Schedule) expand(rule string, rdate, exdate []time.Time, dueAt *time.Time, from, to time.Time) ([]time.Time, error) {
	loc, err := s.Location()
	if err != nil {
		return nil, err
	}
	anchor, err := s.Anchor(dueAt)
	if err != nil {
		return nil, err
	}

	set := &rrule.Set{}
	set.DTStart(anchor.In(loc))

	if rule != "" {
		opt, err := parseRRule(rule)
		if err != nil {
			return nil, fmt.Errorf("parse rule %q: %w", rule, err)
		}
		opt.Dtstart = anchor.In(loc)
		r, err := rrule.NewRRule(*opt)
		if err != nil {
			return nil, fmt.Errorf("build rule %q: %w", rule, err)
		}
		set.RRule(r)
	}
	for _, d := range rdate {
		set.RDate(d.In(loc))
	}
	for _, d := range exdate {
		set.ExDate(d.In(loc))
	}

	out := set.Between(from, to, true)
	for i := range out {
		out[i] = out[i].In(loc)
	}
	return out, nil
}

// NextDueOccurrence is the first occurrence strictly after `after`. Nil when the
// series has ended (an RRULE with UNTIL or COUNT eventually runs out) — which is
// how a rolling item retires itself.
func (s *Schedule) NextDueOccurrence(dueAt *time.Time, after time.Time) (*time.Time, error) {
	// A year is a wide enough window to catch any sane cadence (annual included)
	// while still terminating on an exhausted series.
	occurrences, err := s.DueOccurrences(dueAt, after.Add(time.Second), after.AddDate(2, 0, 0))
	if err != nil {
		return nil, err
	}
	if len(occurrences) == 0 {
		return nil, nil
	}
	return &occurrences[0], nil
}

// ISO8601Duration is a VALARM-style trigger offset: [-]P[nW][nD][T[nH][nM][nS]].
// It is kept as a calendar quantity rather than collapsed into a time.Duration
// because "one day before" and "24 hours before" are different questions on the
// two days a year the clocks move.
type ISO8601Duration struct {
	Negative bool
	Weeks    int
	Days     int
	Hours    int
	Minutes  int
	Seconds  int
}

// ApplyTo shifts t by the duration, calendar-aware: weeks and days move by date
// (so the wall-clock time of day is preserved across a DST transition), while
// hours, minutes and seconds move by elapsed time.
func (d ISO8601Duration) ApplyTo(t time.Time) time.Time {
	sign := 1
	if d.Negative {
		sign = -1
	}
	out := t.AddDate(0, 0, sign*(d.Weeks*7+d.Days))
	return out.Add(time.Duration(sign) * (time.Duration(d.Hours)*time.Hour +
		time.Duration(d.Minutes)*time.Minute +
		time.Duration(d.Seconds)*time.Second))
}

// ParseISO8601Duration parses the subset of ISO 8601 durations iCalendar uses for
// alarm triggers. Months and years are deliberately unsupported: "P1M" before a
// due date is ambiguous by up to three days and nobody means it.
func ParseISO8601Duration(s string) (ISO8601Duration, error) {
	var d ISO8601Duration
	raw := strings.TrimSpace(s)
	if raw == "" {
		return d, fmt.Errorf("empty duration")
	}

	if raw[0] == '-' {
		d.Negative = true
		raw = raw[1:]
	} else if raw[0] == '+' {
		raw = raw[1:]
	}
	if !strings.HasPrefix(raw, "P") {
		return d, fmt.Errorf("must start with P (ISO 8601 duration, e.g. \"-P1D\" or \"-PT2H\")")
	}
	raw = raw[1:]

	datePart, timePart, hasTime := strings.Cut(raw, "T")
	if datePart == "" && !hasTime {
		return d, fmt.Errorf("has no components")
	}

	readComponents := func(part string, units map[byte]*int) error {
		digits := ""
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c >= '0' && c <= '9' {
				digits += string(c)
				continue
			}
			field, ok := units[c]
			if !ok {
				return fmt.Errorf("unsupported unit %q", string(c))
			}
			if digits == "" {
				return fmt.Errorf("unit %q has no value", string(c))
			}
			n, err := strconv.Atoi(digits)
			if err != nil {
				return fmt.Errorf("value %q: %s", boundedtext.Value(digits), boundedtext.ErrorMessage(err))
			}
			*field = n
			digits = ""
		}
		if digits != "" {
			return fmt.Errorf("trailing value %q with no unit", digits)
		}
		return nil
	}

	if err := readComponents(datePart, map[byte]*int{'W': &d.Weeks, 'D': &d.Days}); err != nil {
		return d, fmt.Errorf("in date part: %w (months and years are not supported — they are ambiguous by days)", err)
	}
	if hasTime {
		if timePart == "" {
			return d, fmt.Errorf("has a T separator but no time components")
		}
		if err := readComponents(timePart, map[byte]*int{'H': &d.Hours, 'M': &d.Minutes, 'S': &d.Seconds}); err != nil {
			return d, fmt.Errorf("in time part: %w", err)
		}
	}

	if d.Weeks == 0 && d.Days == 0 && d.Hours == 0 && d.Minutes == 0 && d.Seconds == 0 {
		return d, fmt.Errorf("resolves to zero")
	}
	return d, nil
}
