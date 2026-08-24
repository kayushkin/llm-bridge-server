package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Corpus is the generated mail, on disk.
//
// It is a committed fixture rather than something regenerated on every run. Two
// reasons: re-seeding the demo board should cost nothing and should produce the
// same mailbox twice, and a corpus in version control can be READ in a review —
// generated prose that nobody ever looks at is how a fixture quietly stops
// resembling the thing it stands in for.
type Corpus struct {
	// Model and GeneratedAt record what produced this. A corpus whose prose
	// looks wrong is worth being able to attribute.
	Model       string    `json:"model"`
	GeneratedAt time.Time `json:"generated_at"`
	Account     string    `json:"account"`
	Messages    []Message `json:"messages"`
}

// Message is one email: what was planned, what the model wrote, and — once it
// has been appended — where it lives.
type Message struct {
	// Key is this message's name within the corpus. Threading and card linking
	// both refer to it, so that nothing depends on array position.
	Key  string `json:"key"`
	Kind Kind   `json:"kind"`

	// WorkItem is the title of the card this message concerns, empty for the
	// noise. A TITLE and not a card id: card ids are minted fresh every time the
	// demo board is re-seeded, so an id here would be dead on the next run. This
	// is a key within the fixture, not a join against kanban-store.
	WorkItem string `json:"work_item,omitempty"`

	ThreadKey  string `json:"thread_key"`
	ReplyToKey string `json:"reply_to_key,omitempty"`

	From person   `json:"from"`
	To   []person `json:"to"`

	// HoursAgo is when this was sent, counted back from whenever the corpus is
	// materialized. RELATIVE and not absolute, because the corpus is a fixture
	// that outlives the run that wrote it: an absolute date would still say
	// "19 August" a month from now, putting the mail weeks before the card
	// story it is supposed to explain. Both the mailbox and the board's
	// timeline are built from this one number, so they cannot disagree.
	HoursAgo float64 `json:"hours_ago"`

	// SentAt is HoursAgo resolved against a particular moment. Working state,
	// never stored — storing it is exactly the mistake HoursAgo exists to avoid.
	SentAt time.Time `json:"-"`

	Unread bool `json:"unread"`

	MessageID  string   `json:"message_id"`
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`

	// Subject and Body are the model's work. Everything above is the planner's.
	Subject string `json:"subject"`
	Body    string `json:"body"`

	// Locator is filled in by -append: "account_id:message_id", the ref every
	// consumer uses to point at this message.
	Locator string `json:"locator,omitempty"`
}

// Generated reports whether the model has written this message yet.
func (m Message) Generated() bool { return m.Subject != "" && m.Body != "" }

// loadCorpus reads the corpus and resolves every message's send time against
// now, so the mailbox is always as old as the story it belongs to.
func loadCorpus(path string) (*Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read corpus: %w", err)
	}
	var c Corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode corpus %s: %w", path, err)
	}
	if len(c.Messages) == 0 {
		return nil, fmt.Errorf("corpus %s holds no messages", path)
	}
	now := time.Now().UTC()
	for i := range c.Messages {
		c.Messages[i].SentAt = now.Add(-time.Duration(c.Messages[i].HoursAgo * float64(time.Hour))).Truncate(time.Second)
	}
	return &c, nil
}

// saveCorpus writes the corpus, indented, via a temporary file.
//
// Indented because it is meant to be read in a diff. Via a temp file because a
// generation run that dies halfway through must not leave a truncated corpus
// where the previous good one was.
func saveCorpus(path string, c *Corpus) error {
	encoded, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode corpus: %w", err)
	}
	encoded = append(encoded, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create corpus dir: %w", err)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o644); err != nil {
		return fmt.Errorf("write corpus: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("replace corpus: %w", err)
	}
	return nil
}
