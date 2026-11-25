// Package importclient sends one import to message-store: it starts the run,
// posts conversations, messages and conversation days in batches, and
// finishes it. A dry run counts what it would send and posts nothing.
package importclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	messagestore "github.com/kayushkin/message-store"
)

// BatchSize is how many records go in one POST.
const BatchSize = 2000

// Client is one import in progress.
type Client struct {
	StoreURL string
	DryRun   bool

	http     *http.Client
	importID string
	batch    messagestore.ImportBatch
	pending  int
	// sentConversations is each conversation already sent in this run, so
	// one named by ten thousand messages goes once.
	sentConversations map[string]bool
	// Read counts the records handed to the client, by kind.
	Read map[string]int
	// Written sums what the store did.
	Written messagestore.ImportCounts
}

// New makes a client for one import.
func New(storeURL string, dryRun bool) *Client {
	return &Client{StoreURL: strings.TrimRight(storeURL, "/"), DryRun: dryRun, http: &http.Client{Timeout: 10 * time.Minute},
		sentConversations: map[string]bool{}, Read: map[string]int{}}
}

// ImportID is the store's id for this run, once begun.
func (c *Client) ImportID() string { return c.importID }

func (c *Client) post(path string, body, answer any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	response, err := c.http.Post(c.StoreURL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(response.Body)
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: %s: %s", path, response.Status, strings.TrimSpace(string(content)))
	}
	if answer != nil {
		return json.Unmarshal(content, answer)
	}
	return nil
}

// Begin starts the run in the store.
func (c *Client) Begin(importer, description string) error {
	if c.DryRun {
		return nil
	}
	var run messagestore.Import
	if err := c.post("/imports", messagestore.ImportStart{Importer: importer, Description: description}, &run); err != nil {
		return err
	}
	c.importID = run.ID
	return nil
}

// AddConversation queues a conversation once per run; a later call with the
// same source and source_ref sends it again only to carry new participants.
func (c *Client) AddConversation(conversation messagestore.ImportConversation) error {
	key := conversation.Source + "\x00" + conversation.SourceRef
	if !c.sentConversations[key] {
		c.Read["conversations"]++
	}
	c.sentConversations[key] = true
	c.batch.Conversations = append(c.batch.Conversations, conversation)
	return c.queued()
}

// AddMessage queues one message.
func (c *Client) AddMessage(message messagestore.ImportMessage) error {
	c.Read["messages"]++
	c.batch.Messages = append(c.batch.Messages, message)
	return c.queued()
}

// AddConversationDay queues one conversation day.
func (c *Client) AddConversationDay(day messagestore.ImportConversationDay) error {
	c.Read["conversation_days"]++
	c.batch.ConversationDays = append(c.batch.ConversationDays, day)
	return c.queued()
}

// AddSupersession queues the retirement of a row in favor of a stored
// message.
func (c *Client) AddSupersession(supersession messagestore.ImportSupersession) error {
	c.Read["supersessions"]++
	c.batch.Supersessions = append(c.batch.Supersessions, supersession)
	return c.queued()
}

// AddConversationMerge queues the merge of one conversation into another.
func (c *Client) AddConversationMerge(merge messagestore.ImportConversationMerge) error {
	c.Read["conversation_merges"]++
	c.batch.ConversationMerges = append(c.batch.ConversationMerges, merge)
	return c.queued()
}

func (c *Client) queued() error {
	c.pending++
	if c.pending >= BatchSize {
		return c.Flush()
	}
	return nil
}

// Flush posts what is queued.
func (c *Client) Flush() error {
	if c.pending == 0 {
		return nil
	}
	batch := c.batch
	c.batch, c.pending = messagestore.ImportBatch{}, 0
	if c.DryRun {
		return nil
	}
	var counts messagestore.ImportCounts
	if err := c.post("/imports/"+c.importID+"/batches", batch, &counts); err != nil {
		return err
	}
	c.Written.ConversationsCreated += counts.ConversationsCreated
	c.Written.ConversationsUpdated += counts.ConversationsUpdated
	c.Written.MessagesCreated += counts.MessagesCreated
	c.Written.MessagesUpdated += counts.MessagesUpdated
	c.Written.MessagesUnchanged += counts.MessagesUnchanged
	c.Written.DaysCreated += counts.DaysCreated
	c.Written.DaysUpdated += counts.DaysUpdated
	c.Written.MessagesSuperseded += counts.MessagesSuperseded
	c.Written.MessagesSupersededSkipped += counts.MessagesSupersededSkipped
	c.Written.ConversationsMerged += counts.ConversationsMerged
	c.Written.ConversationMergesSkipped += counts.ConversationMergesSkipped
	return nil
}

// Finish flushes and marks the run done.
func (c *Client) Finish() error {
	if err := c.Flush(); err != nil {
		return err
	}
	if c.DryRun {
		return nil
	}
	return c.post("/imports/"+c.importID+"/finish", struct{}{}, nil)
}

// Report is one line for the end of a run.
func (c *Client) Report() string {
	if c.DryRun {
		return fmt.Sprintf("dry run: read %d conversations, %d messages, %d conversation days, %d conversation merges; nothing sent",
			c.Read["conversations"], c.Read["messages"], c.Read["conversation_days"], c.Read["conversation_merges"])
	}
	w := c.Written
	return fmt.Sprintf("%s: conversations %d new, %d changed; messages %d new, %d changed, %d unchanged, %d superseded, %d skipped as already superseded; days %d new, %d changed; conversations merged %d, merges naming a conversation not held %d",
		c.importID, w.ConversationsCreated, w.ConversationsUpdated, w.MessagesCreated, w.MessagesUpdated, w.MessagesUnchanged, w.MessagesSuperseded,
		w.MessagesSupersededSkipped, w.DaysCreated, w.DaysUpdated, w.ConversationsMerged, w.ConversationMergesSkipped)
}
