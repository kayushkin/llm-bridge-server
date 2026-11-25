package journalstore

import (
	"fmt"
	"strings"
)

// The kind and visibility vocabularies are served (GET /entry-kinds, GET
// /visibilities) rather than inferred from the rows, so no caller builds a
// filter out of whatever values happen to exist.

const (
	KindJournal = "journal" // a dated diary entry; the only kind whose title may be empty
	KindPost    = "post"    // something meant to be published
	KindNote    = "note"    // a note kept for later; Samsung Notes imports land here
	KindAbout   = "about"   // a note about one person or thing, named by about_ref
	// KindTimeline is a timeline: its dated items live in timeline_items. Its
	// about_ref names the person it follows; an event timeline (a trip, a
	// job) names nothing.
	KindTimeline = "timeline"
	// KindTranscript is a record of a conversation the user took part in,
	// imported from an archive (a Skype log), kept so a timeline can cite the
	// conversation and the reader can read it whole.
	KindTranscript = "transcript"
)

// EntryKinds is every kind an entry can be.
var EntryKinds = []string{KindJournal, KindPost, KindNote, KindAbout, KindTimeline, KindTranscript}

// How exactly a timeline item's occurred_at is known. The time is stored to
// the second either way; precision says how much of it to believe and show.
const (
	PrecisionMinute = "minute" // a message's timestamp, a note's file-name time
	PrecisionDay    = "day"    // the date is known, the time is not
	PrecisionMonth  = "month"  // occurred_at is the first of the month
	PrecisionYear   = "year"   // occurred_at is January 1st
)

// TimePrecisions is every precision a timeline item can have.
var TimePrecisions = []string{PrecisionMinute, PrecisionDay, PrecisionMonth, PrecisionYear}

const (
	VisibilityPrivate   = "private"   // the default: only the user
	VisibilityDraft     = "draft"     // headed for publication, not there yet
	VisibilityPublished = "published" // served at GET /published
)

// Visibilities is every visibility an entry can have.
var Visibilities = []string{VisibilityPrivate, VisibilityDraft, VisibilityPublished}

func normalizeFromVocabulary(raw string, vocabulary []string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	for _, word := range vocabulary {
		if key == word {
			return word, true
		}
	}
	return "", false
}

// NormalizeKind resolves a caller's word to a canonical kind.
func NormalizeKind(raw string) (string, bool) { return normalizeFromVocabulary(raw, EntryKinds) }

// NormalizeVisibility resolves a caller's word to a canonical visibility.
func NormalizeVisibility(raw string) (string, bool) {
	return normalizeFromVocabulary(raw, Visibilities)
}

// NormalizePrecision resolves a caller's word to a canonical precision.
func NormalizePrecision(raw string) (string, bool) {
	return normalizeFromVocabulary(raw, TimePrecisions)
}

func errUnknownKind(raw string) error {
	return fmt.Errorf("%w: unknown kind %q: use one of %s", ErrInvalidEntry, raw, strings.Join(EntryKinds, ", "))
}

func errUnknownVisibility(raw string) error {
	return fmt.Errorf("%w: unknown visibility %q: use one of %s", ErrInvalidEntry, raw, strings.Join(Visibilities, ", "))
}
