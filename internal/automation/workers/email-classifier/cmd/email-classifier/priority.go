package main

import (
	"fmt"
	"strings"
	"time"
)

// ============================ priority ============================
//
// Priority answers the one question the three classification axes could not:
// out of everything on this board, what should a person look at first?
//
// Urgency alone could not answer it. The model writes urgency once, at
// classification time, from whatever deadline the mail stated — and then nothing
// revisits it unless new mail happens to arrive for that same card. Three days
// in, that had already gone wrong in both directions: a concert "tonight" still
// tagged critical two days after the show, an event on Aug 14 still tagged
// normal on Aug 13. A frozen adjective cannot track a moving date.
//
// So the deadline itself is now stored — noteboard's due_at — and the judgment
// is derived from it on every tick by the pure functions below. Nothing here
// calls a model. Re-scoring the entire board is arithmetic, and that is what
// makes it affordable to redo every fifteen minutes instead of only when mail
// happens to land.
//
// The split also gives each axis one meaning. Urgency used to conflate time with
// consequence ("critical = deadline inside 24h, OR money at stake"), so a card
// could be critical for two unrelated reasons and you could not tell which.
// Here urgency is time alone, stakes is consequence alone, and priority is the
// only thing that combines them.

// stakes is what it costs to miss a card, independent of when it comes due.
// Deadline pressure decays and recovers; stakes do not, which is why the two are
// scored apart and combined only at the end.
type stakes int

const (
	// stakesNone is a gate, not a low weight: reference mail that no deadline
	// may ever lift. Most of this board is bucket cards, and a promotion
	// counting down from 24 hours is still a promotion.
	stakesNone stakes = iota
	stakesMinor
	stakesReal
	stakesSevere
)

func (s stakes) String() string {
	switch s {
	case stakesSevere:
		return "severe"
	case stakesReal:
		return "real"
	case stakesMinor:
		return "minor"
	default:
		return "none"
	}
}

// stakesOf combines the two axes.
//
// An action of read/file/none gates the card to zero no matter how weighty its
// category. That asymmetry is the whole point: "Zillow rental listings — Reno
// area" is cat:housing with action:none, and simply taking the higher of the two
// axes would float a listings bucket to the top of the board on the strength of
// the category alone. Nothing is owed on it, so nothing should lift it.
//
// An action outside the vocabulary does NOT gate — it falls through to the
// category. An unrecognized value means the classifier misbehaved, and silently
// scoring it to zero would bury exactly the card worth seeing.
func stakesOf(category, action string) stakes {
	actionScore, actionKnown := active.ActionStakes[action]
	if actionKnown && actionScore == stakesNone {
		return stakesNone
	}
	categoryScore := active.CategoryStakes[category]
	if actionScore > categoryScore {
		return actionScore
	}
	return categoryScore
}

// deadlineBucket is how much time is left, coarsened to the granularity a person
// actually triages at. Ordered by pressure so the priority table below can be a
// dense array rather than a map with holes in it.
type deadlineBucket int

const (
	deadlineNone deadlineBucket = iota
	deadlineFar
	deadlineWeek
	deadlineSoon
	deadlineToday
	deadlineOverdue
	deadlineBucketCount
)

// bucketForDue reads a real deadline.
//
// Whether the deadline has PASSED is an instant comparison, but how far away a
// future one is gets measured in calendar days rather than hours. Mail states
// most deadlines as bare dates, which parseDueDate resolves to the end of the
// day; measuring those in raw hours makes "tomorrow" score as twenty-six hours
// out and land a bucket too low, and it makes the answer swing on whether the
// sweep happens to run at 22:00 or at 02:00. Days are also simply how a person
// reads "Aug 14" — it is tomorrow, all day, at any hour of today.
func bucketForDue(due, now time.Time) deadlineBucket {
	if due.Before(now) {
		return deadlineOverdue
	}
	switch daysUntil := calendarDaysBetween(now, due); {
	case daysUntil <= 1:
		return deadlineToday
	case daysUntil <= 3:
		return deadlineSoon
	case daysUntil <= 14:
		return deadlineWeek
	default:
		return deadlineFar
	}
}

// calendarDaysBetween counts midnights crossed going from one instant to a later
// one, in the zone `from` carries. Same day is 0, tomorrow is 1.
func calendarDaysBetween(from, to time.Time) int {
	loc := from.Location()
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	toLocal := to.In(loc)
	toDay := time.Date(toLocal.Year(), toLocal.Month(), toLocal.Day(), 0, 0, 0, 0, loc)
	return int(toDay.Sub(fromDay).Hours() / 24)
}

// urgencyBuckets reads the model's stored urgency tag as a deadline bucket, for
// cards that carry no due date.
//
// Every card written before this change has no due_at, so without this fallback
// the first sweep would score the whole board as if nothing were ever due and
// flatten three days of the model's judgment in one pass.
var urgencyBuckets = map[string]deadlineBucket{
	"expired":  deadlineOverdue,
	"critical": deadlineToday,
	"high":     deadlineSoon,
	"normal":   deadlineWeek,
	"low":      deadlineNone,
}

// urgencyForBucket maps a bucket back onto the urgency vocabulary, so the
// board's existing urgency filter keeps telling the truth once a real deadline
// starts driving it.
var urgencyForBucket = map[deadlineBucket]string{
	deadlineOverdue: "expired",
	deadlineToday:   "critical",
	deadlineSoon:    "high",
	deadlineWeek:    "normal",
	deadlineFar:     "low",
	deadlineNone:    "low",
}

// maxPriority is the top of the scale. Higher is more urgent, and 0 means
// "nothing is owed here" rather than "not yet scored" — though the two coincide
// on a board that has never been swept, which is the safe direction to be wrong
// in: an unscored card sinks rather than shouting.
const maxPriority = 5

// priorityMatrix is stakes × deadline → priority. A dense array rather than
// nested maps, so every combination is stated exactly once and none can be
// missing.
//
// Two entries carry most of the design:
//
//   - severe + overdue is 5, the top of the scale. A missed rent payment is not
//     history, it is a late fee still growing. The old scheme collapsed this
//     into urgency:expired alongside a concert that had already happened.
//   - minor + overdue is 1, near the bottom. That same concert is genuinely
//     over, and sinking it is the correct answer — but it stays above 0 so it
//     remains visibly a thing that lapsed rather than blending into the buckets.
var priorityMatrix = [4][deadlineBucketCount]int{
	//             none far week soon today overdue
	stakesNone:   {0, 0, 0, 0, 0, 0},
	stakesMinor:  {1, 1, 1, 2, 3, 1},
	stakesReal:   {1, 2, 2, 3, 4, 4},
	stakesSevere: {2, 2, 3, 4, 5, 5},
}

// isMootOncePassed reports whether a passed deadline settles the card rather
// than escalating it.
//
// Such cards sink to overduePassedPriority rather than to zero: the user asked
// to archive lapsed items by hand, so they must stay visibly lapsed rather than
// blending into the buckets that never mattered.
func isMootOncePassed(category, action string) bool {
	return active.MootCategoriesWhenPassed[category] || active.MootActionsWhenPassed[action]
}

// overduePassedPriority is where a lapsed but consequence-free card lands: above
// the floor, below anything still live.
const overduePassedPriority = 1

// cardScore is the full verdict on one card: what it should be worth, and what
// its urgency tag should say now.
type cardScore struct {
	Priority int
	Stakes   stakes
	Bucket   deadlineBucket
	// Urgency is the recomputed urgency value, empty when the card carries no
	// due date and the stored tag must therefore be left alone.
	Urgency string
}

// scoreCard is the whole scoring rule in one place.
//
// When due is nil the card's stored urgency stands in for the deadline and is
// deliberately NOT rewritten: without a date there is nothing better to replace
// it with, and overwriting the model's reading with a derived "low" would
// destroy information rather than refine it. noteboard's revision snapshots do
// not cover due_at, so that loss would also be unrecoverable.
func scoreCard(category, action, storedUrgency string, due *time.Time, now time.Time) cardScore {
	s := stakesOf(category, action)

	var bucket deadlineBucket
	var urgency string
	if due != nil {
		bucket = bucketForDue(*due, now)
		urgency = urgencyForBucket[bucket]
	} else {
		// An urgency outside the vocabulary falls to deadlineNone, which scores
		// the card on stakes alone — the honest answer when the only time signal
		// present is unreadable.
		bucket = urgencyBuckets[storedUrgency]
	}

	priority := priorityMatrix[s][bucket]
	if bucket == deadlineOverdue && isMootOncePassed(category, action) && priority > overduePassedPriority {
		priority = overduePassedPriority
	}

	return cardScore{
		Priority: priority,
		Stakes:   s,
		Bucket:   bucket,
		Urgency:  urgency,
	}
}

// parseDueDate reads the due date the model reported.
//
// Three shapes are accepted because mail states deadlines all three ways:
//
//   - a bare calendar date, "2026-08-14" ("due Aug 14")
//   - a zoned instant, "2026-08-14T19:30:00-07:00"
//   - a naive local datetime, "2026-08-14T19:30:00" ("the 7:30 showing")
//
// The naive form is not a concession. Asked for a deadline the mail states as a
// wall-clock time, a model returns one far more often than it invents an offset,
// and the first live backfill lost two cards to exactly that — "NNBA Dog Days of
// Summer performance Aug 14" and "Confirm Nevada DMV appointment Sept 5 8am"
// both came back as bare datetimes and were dropped. Mail states local times, so
// reading them in the local zone is also the correct interpretation rather than
// merely a lenient one.
//
// A bare date resolves to the END of that day, not its midnight — a bill due
// "Aug 14" is not overdue at 00:01 on Aug 14, and rounding the other way would
// mark a whole class of cards expired a day early. A datetime is taken at the
// instant given, since a stated time is already precise.
//
// Anything else is an error rather than a zero time. A silently dropped deadline
// is the failure this entire file exists to correct.
func parseDueDate(raw string, loc *time.Location) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty due date")
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", raw, loc); err == nil {
		return t, nil
	}
	if day, err := time.ParseInLocation("2006-01-02", raw, loc); err == nil {
		return day.Add(24*time.Hour - time.Second), nil
	}
	return time.Time{}, fmt.Errorf("due date %q is not YYYY-MM-DD, YYYY-MM-DDTHH:MM:SS, or RFC3339", raw)
}
