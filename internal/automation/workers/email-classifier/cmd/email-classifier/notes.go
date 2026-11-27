package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ============================ routing notes ============================
//
// A routing note is something the user knows about a sender or a message that
// the message itself does not say: "this is my landlord", "this address only
// ever sends receipts", "the Tuesday one is the real invoice".
//
// It is deliberately NOT a rule. The classifier had a learned sender rule once,
// which filed an address's mail onto a card without fetching or reading it, and
// it was removed because one address is not one topic. A note goes the other
// way: it is written by a person, it says WHY, and it reaches the model as part
// of the digest rather than short-circuiting it. The model still reads the
// message and still decides. What changes is that it decides knowing what the
// user knows.
//
// Notes live in noteboard, which is this host's canonical store for notes —
// keeping them anywhere else would be a second place to look for the same kind
// of thing. Each is an ordinary item of type `note`, carrying routingNoteTag so
// they can be listed in one call, and one entity ref in `links` naming what it
// is about:
//
//	email_sender:landlord@example.com        every message from that address
//	email:gmail-personal:19fc857e5e15e426    one specific message
//
// That ref is the same string kanban-store uses for the card links the
// classifier already writes, so a note and a card link point at the same entity
// in the same vocabulary.

const (
	// routingNoteTag marks a noteboard item as a routing note. Every note
	// carries it, and it is what makes them one list call rather than a search.
	routingNoteTag = "email-routing-note"

	// entityTypeSenderPrefix and entityTypeMessagePrefix are the two link forms
	// a note may carry. They are the kanban-store entity types spelled as a ref
	// prefix, so `email_sender:x@y.com` reads the same here as it does there.
	entityTypeSenderPrefix  = linkTypeEmailSender + ":"
	entityTypeMessagePrefix = linkTypeEmail + ":"
)

// routingNote is one note and what it is about.
type routingNote struct {
	ItemID    string
	EntityRef string
	Title     string
	Text      string
	UpdatedAt time.Time
}

// routingNotes is every note, indexed by what it is about.
//
// Two indexes rather than one map keyed on the raw ref, because the two are
// looked up by different things: a message by its locator, a sender by the
// address on the message being filed.
type routingNotes struct {
	bySender  map[string]routingNote
	byMessage map[string]routingNote
	all       []routingNote
}

func (n *routingNotes) forSender(address string) (routingNote, bool) {
	if n == nil {
		return routingNote{}, false
	}
	note, ok := n.bySender[strings.ToLower(strings.TrimSpace(address))]
	return note, ok
}

func (n *routingNotes) forMessage(locator string) (routingNote, bool) {
	if n == nil {
		return routingNote{}, false
	}
	note, ok := n.byMessage[locator]
	return note, ok
}

func (n *routingNotes) count() int {
	if n == nil {
		return 0
	}
	return len(n.all)
}

// ============================ noteboard client ============================

type noteboardClient struct {
	BaseURL string
	HTTP    *http.Client
}

func newNoteboardClient(baseURL string) *noteboardClient {
	return &noteboardClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// noteboardItem is the subset of an item this command reads. noteboard sends
// far more; naming only what is used keeps a schema change from silently
// altering behavior here.
type noteboardItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Tags      []string  `json:"tags"`
	Links     []string  `json:"links"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c *noteboardClient) do(method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("noteboard %s %s: %s — %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// loadRoutingNotes reads every routing note in one call and indexes it.
//
// One call per tick regardless of how much mail is in the batch: the alternative
// — asking per sender as each message is classified — would put a round trip in
// the middle of the filing loop for a lookup that almost always misses.
func loadRoutingNotes(c *noteboardClient) (*routingNotes, error) {
	// noteboard answers a list route with a bare JSON array, not an object
	// wrapping one. Decoding into a wrapper here failed against the live service
	// while passing against a fake that invented the wrapper — so the fake in
	// notes_test.go now serves an array too.
	var items []noteboardItem
	path := "/api/items?type=note&tag=" + url.QueryEscape(routingNoteTag) + "&limit=500"
	if err := c.do("GET", path, nil, &items); err != nil {
		return nil, err
	}
	notes := &routingNotes{
		bySender:  map[string]routingNote{},
		byMessage: map[string]routingNote{},
	}
	for _, item := range items {
		for _, ref := range item.Links {
			note := routingNote{
				ItemID: item.ID, EntityRef: ref, Title: item.Title,
				Text: strings.TrimSpace(item.Body), UpdatedAt: item.UpdatedAt,
			}
			switch {
			case strings.HasPrefix(ref, entityTypeSenderPrefix):
				address := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ref, entityTypeSenderPrefix)))
				notes.bySender[address] = note
			case strings.HasPrefix(ref, entityTypeMessagePrefix):
				notes.byMessage[strings.TrimPrefix(ref, entityTypeMessagePrefix)] = note
			default:
				// Not ours. A note may legitimately link to other things.
				continue
			}
			notes.all = append(notes.all, note)
		}
	}
	sort.Slice(notes.all, func(i, j int) bool { return notes.all[i].EntityRef < notes.all[j].EntityRef })
	return notes, nil
}

// upsertRoutingNote writes the note for one entity, replacing the text if a note
// for that entity already exists.
//
// Replacing rather than appending is deliberate: a routing note is the user's
// current standing instruction about that mail, not a log of what they have
// thought about it. noteboard keeps the previous text in item_revisions, so the
// old wording is recoverable without keeping it in the live note.
func upsertRoutingNote(c *noteboardClient, existing *routingNotes, entityRef, text string) (string, bool, error) {
	var prior routingNote
	var found bool
	switch {
	case strings.HasPrefix(entityRef, entityTypeSenderPrefix):
		prior, found = existing.forSender(strings.TrimPrefix(entityRef, entityTypeSenderPrefix))
	case strings.HasPrefix(entityRef, entityTypeMessagePrefix):
		prior, found = existing.forMessage(strings.TrimPrefix(entityRef, entityTypeMessagePrefix))
	default:
		return "", false, fmt.Errorf("entity ref %q must start with %q or %q", entityRef, entityTypeSenderPrefix, entityTypeMessagePrefix)
	}

	if found {
		if err := c.do("PATCH", "/api/items/"+prior.ItemID, map[string]any{"body": text}, nil); err != nil {
			return "", false, err
		}
		return prior.ItemID, false, nil
	}

	var created noteboardItem
	err := c.do("POST", "/api/items", map[string]any{
		"type":  "note",
		"title": "Routing note — " + entityRef,
		"body":  text,
		"tags":  []string{routingNoteTag, "email"},
		"links": []string{entityRef},
	}, &created)
	if err != nil {
		return "", false, err
	}
	return created.ID, true, nil
}

// deleteRoutingNote removes the note for an entity. noteboard's DELETE is
// reversible, so this is a hide rather than a destruction.
func deleteRoutingNote(c *noteboardClient, note routingNote) error {
	return c.do("DELETE", "/api/items/"+note.ItemID, nil, nil)
}

// ============================ digest rendering ============================

// renderNoteForDigest returns the line describing a note, or "" when there is
// none. The sender note and the message note are both offered when both exist:
// they say different things, and the more specific one does not replace the
// standing instruction about the address.
func renderNoteForDigest(notes *routingNotes, em pendingEmail) string {
	var lines []string
	if note, ok := notes.forSender(senderKey(em.Meta.From)); ok && note.Text != "" {
		lines = append(lines, "user_note_about_this_sender: "+collapseWhitespace(note.Text))
	}
	if note, ok := notes.forMessage(em.Locator); ok && note.Text != "" {
		lines = append(lines, "user_note_about_this_message: "+collapseWhitespace(note.Text))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
