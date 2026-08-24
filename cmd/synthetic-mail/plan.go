package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// workItem is one card on the board, as the planner needs it.
type workItem struct {
	Title string
	Body  string
	// Action is the card's `action:` tag — task, investigate, cancel, write,
	// triage, none. It decides what KIND of email would have created this card,
	// which is how the corpus stays consistent with the board rather than
	// inventing a second story about the same work.
	Action string
	// ArrivedAt is when this card's mail arrived, taken from the card's own
	// timeline. Read rather than invented: the card already records when its
	// work started, and a corpus that disagreed with it would put every message
	// at odds with the timeline it is supposed to explain.
	ArrivedAt time.Time
}

// openingKindForAction maps a card's action onto the kind of email that would
// have produced it.
var openingKindForAction = map[string]Kind{
	"task":        KindTaskRequest,
	"investigate": KindStatusCheck,
	"cancel":      KindCancellation,
	"write":       KindIncident,
	"none":        KindBulkNotification,
	"triage":      KindUnfiled,
}

// followUpKindForOpening maps an opening onto what plausibly comes back. An
// empty entry means that kind of thread ends with one message.
var followUpKindForOpening = map[Kind]Kind{
	KindTaskRequest: KindReviewResponse,
	KindStatusCheck: KindVendorReply,
	KindIncident:    KindScopeChange,
}

// plan lays out every message before a single word is generated.
//
// Separating the plan from the writing is what makes threading possible at all:
// a reply needs its parent's Message-ID, and that has to exist before the reply
// is written. It also means a failed generation run can be resumed against the
// same plan instead of producing a different mailbox.
func plan(items []workItem, noiseCount int, seed int64, now time.Time) ([]Message, error) {
	// Every message's HoursAgo is set relative to this one moment, at the end.
	if len(items) == 0 {
		return nil, fmt.Errorf("no work items to write mail about")
	}
	random := rand.New(rand.NewSource(seed))

	// Oldest first, so message keys run in the order the mail arrived and a
	// corpus diff reads chronologically.
	sorted := append([]workItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ArrivedAt.Before(sorted[j].ArrivedAt) })

	var messages []Message
	next := 1
	key := func() string {
		k := fmt.Sprintf("msg-%03d", next)
		next++
		return k
	}

	for _, item := range sorted {
		openingKind, known := openingKindForAction[item.Action]
		if !known {
			// A card whose action nobody recognizes still gets mail — it is a
			// request until something says otherwise — but say so, because a
			// silently-defaulted corpus is how a vocabulary drifts unnoticed.
			fmt.Printf("  note: %q has action %q, which maps to no email kind; treating it as a request\n",
				item.Title, item.Action)
			openingKind = KindTaskRequest
		}

		threadKey := fmt.Sprintf("thread-%s", strings.ToLower(strings.ReplaceAll(shortTitle(item.Title), " ", "-")))
		opening := newMessage(key(), openingKind, item, threadKey, "", item.ArrivedAt, random)
		messages = append(messages, opening)

		followUpKind, hasFollowUp := followUpKindForOpening[openingKind]
		if !hasFollowUp {
			continue
		}
		// A reply lands somewhere between four hours and three days after what
		// it answers, and never in the future — a corpus with tomorrow's mail in
		// it breaks every "how long has this been waiting" figure downstream.
		gap := time.Duration(4+random.Intn(68)) * time.Hour
		replyAt := opening.SentAt.Add(gap)
		if !replyAt.Before(now) {
			replyAt = now.Add(-time.Duration(1+random.Intn(3)) * time.Hour)
		}
		if !replyAt.After(opening.SentAt) {
			// The opening is itself recent enough that there is no room for a
			// reply. Leave the thread as one message rather than inventing a
			// reply that arrives before the mail it answers.
			continue
		}
		reply := newMessage(key(), followUpKind, item, threadKey, opening.MessageID, replyAt, random)
		reply.ReplyToKey = opening.Key
		reply.References = []string{opening.MessageID}
		messages = append(messages, reply)
	}

	// Noise: mail about no work item at all. A classifier that has only ever
	// seen filable mail has never been asked the question it exists to answer.
	for i := 0; i < noiseCount; i++ {
		kind := KindBulkNotification
		if i%3 == 2 {
			kind = KindUnfiled
		}
		sentAt := now.Add(-time.Duration(2+random.Intn(160)) * time.Hour)
		threadKey := fmt.Sprintf("thread-noise-%02d", i+1)
		messages = append(messages, newMessage(key(), kind, workItem{}, threadKey, "", sentAt, random))
	}

	sort.SliceStable(messages, func(i, j int) bool { return messages[i].SentAt.Before(messages[j].SentAt) })
	for i := range messages {
		messages[i].HoursAgo = roundTo(now.Sub(messages[i].SentAt).Hours(), 2)
	}
	return messages, nil
}

// newMessage fills in everything a sender and a mail system decide, leaving
// only the subject and body for the model.
func newMessage(key string, kind Kind, item workItem, threadKey, inReplyTo string, sentAt time.Time, random *rand.Rand) Message {
	spec := specFor(kind)
	sender := personByKey(spec.Senders[random.Intn(len(spec.Senders))])

	return Message{
		Key:       key,
		Kind:      kind,
		WorkItem:  item.Title,
		ThreadKey: threadKey,
		From:      sender,
		To:        []person{owner},
		SentAt:    sentAt.UTC().Truncate(time.Second),
		// Everything unread except the automated bulk, which nobody opens. This
		// is what gives the inbox a believable unread count rather than a
		// uniform one.
		Unread:    kind != KindBulkNotification,
		MessageID: fmt.Sprintf("%s.%d@northwind-eng.example", key, sentAt.UTC().Unix()),
		InReplyTo: inReplyTo,
	}
}

// roundTo keeps the stored offset readable in a diff rather than carrying
// fifteen digits of float noise.
func roundTo(v float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(v*scale) / scale
}

// shortTitle trims a card title down to something usable inside an identifier.
func shortTitle(title string) string {
	words := strings.Fields(title)
	if len(words) > 4 {
		words = words[:4]
	}
	cleaned := make([]string, 0, len(words))
	for _, w := range words {
		var b strings.Builder
		for _, r := range w {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			cleaned = append(cleaned, b.String())
		}
	}
	if len(cleaned) == 0 {
		return "item"
	}
	return strings.Join(cleaned, " ")
}
