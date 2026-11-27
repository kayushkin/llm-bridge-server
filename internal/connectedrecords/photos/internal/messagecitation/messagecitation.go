// Package messagecitation finds, in message-store, the chat message a photo
// was sent in, and the conversation that holds it: the record a photo from a
// chat archive cites.
package messagecitation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Store is message-store's read API.
type Store struct {
	Base   string
	Client *http.Client
}

// Message is a stored chat message: its id, its conversation, and the
// person who sent it when message-store knows them.
type Message struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	SenderPersonID string `json:"sender_person_id"`
}

// Participant is one handle in a conversation and its person.
type Participant struct {
	PersonID string `json:"person_id"`
	IsSelf   bool   `json:"is_self"`
}

// Conversation is a stored chat: its title and everyone in it.
type Conversation struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Participants []Participant `json:"participants"`
}

// ErrNotOne says message-store holds no message, or several, under a ref.
type ErrNotOne struct{ Count int }

func (e ErrNotOne) Error() string { return fmt.Sprintf("%d messages are filed under it", e.Count) }

// MessageFiledAs returns the one message an importer filed under source and
// its own ref (a Discord message id under "discord").
func (s Store) MessageFiledAs(source, ref string) (Message, error) {
	var page struct {
		Messages []Message `json:"messages"`
	}
	if err := s.get("/messages?"+url.Values{"source": {source}, "source_ref": {ref}, "limit": {"2"}}.Encode(), &page); err != nil {
		return Message{}, err
	}
	if len(page.Messages) != 1 {
		return Message{}, ErrNotOne{Count: len(page.Messages)}
	}
	return page.Messages[0], nil
}

// MessageByID returns one stored message.
func (s Store) MessageByID(id string) (Message, error) {
	var message Message
	return message, s.get("/messages/"+url.PathEscape(id), &message)
}

// ConversationByID returns one stored conversation.
func (s Store) ConversationByID(id string) (Conversation, error) {
	var conversation Conversation
	return conversation, s.get("/conversations/"+url.PathEscape(id), &conversation)
}

func (s Store) get(path string, into any) error {
	response, err := s.Client.Get(strings.TrimRight(s.Base, "/") + path)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("message-store %s answered %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, into)
}
