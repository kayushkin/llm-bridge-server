package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Who sent a chat link message, by id. Each inbound message carries the
// platform's id of its sender; before it is delivered, people-store is asked
// which person owns that account and what principal the person has. The
// session is told the message is the operator's only when that principal is
// the one the session runs as. Names never decide it.

// chatLinkIdentityKinds maps a link's platform to the people-store identity
// kind its user ids are filed under. A platform not listed has no kind, and
// its senders are labeled as nobody known.
var chatLinkIdentityKinds = map[string]string{
	"discord": "discord",
}

// resolveChatLinkSenders reads the sender of every message not read yet and
// answers the messages with their senders filled in. When people-store
// cannot answer, the delivery is recorded as a failed attempt with the
// reason, and the error is answered: the sweep tries again, and no message
// goes in with a guessed sender.
func (s *Server) resolveChatLinkSenders(ctx context.Context, link store.ChatLink, messages []store.ChatLinkMessage) ([]store.ChatLinkMessage, error) {
	resolved := make([]store.ChatLinkMessage, len(messages))
	for index, message := range messages {
		resolved[index] = message
		if !message.SenderResolvedAt.IsZero() {
			continue
		}
		sender, err := s.chatLinks.lookUpSender(ctx, link.Platform, message.SenderPlatformUserID)
		now := s.chatLinks.now()
		if err != nil {
			reason := fmt.Sprintf("could not read who sent %s from people-store: %v", message.ID, err)
			log.Printf("[chat-links] ERROR: %s: %s", link.ID, reason)
			ids := make([]string, len(messages))
			for index, message := range messages {
				ids[index] = message.ID
			}
			if recordErr := s.store.RecordChatLinkDeliveryAttemptFailed(ids, reason, now); recordErr != nil {
				log.Printf("[chat-links] ERROR: %s: record the failed delivery: %v", link.ID, recordErr)
			}
			return nil, err
		}
		if err := s.store.RecordChatLinkMessageSender(message.ID, sender, now); err != nil {
			log.Printf("[chat-links] ERROR: %s: record the sender of %s: %v", link.ID, message.ID, err)
			return nil, err
		}
		resolved[index].SenderPersonID = sender.PersonID
		resolved[index].SenderPrincipalID = sender.PrincipalID
		resolved[index].SenderPersonDisplayName = sender.PersonDisplayName
		resolved[index].SenderResolvedAt = now
	}
	return resolved, nil
}

// lookUpSender asks people-store who owns a platform account. An account
// nobody owns, or one with no id or no identity kind, is an empty sender and
// no error; people-store failing to answer is an error.
func (c *chatLinks) lookUpSender(ctx context.Context, platform, platformUserID string) (store.ChatLinkMessageSender, error) {
	kind := chatLinkIdentityKinds[platform]
	if kind == "" || platformUserID == "" {
		return store.ChatLinkMessageSender{}, nil
	}
	query := url.Values{"kind": {kind}, "value": {platformUserID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.peopleStoreURL, "/")+"/identities/resolve?"+query.Encode(), nil)
	if err != nil {
		return store.ChatLinkMessageSender{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return store.ChatLinkMessageSender{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return store.ChatLinkMessageSender{}, err
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return store.ChatLinkMessageSender{}, nil
	default:
		return store.ChatLinkMessageSender{}, fmt.Errorf("people-store answered %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var answer struct {
		Person struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			PrincipalID string `json:"principal_id"`
		} `json:"person"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return store.ChatLinkMessageSender{}, fmt.Errorf("people-store's answer: %w", err)
	}
	if answer.Person.ID == "" {
		return store.ChatLinkMessageSender{}, fmt.Errorf("people-store answered 200 with no person: %s", strings.TrimSpace(string(body)))
	}
	return store.ChatLinkMessageSender{PersonID: answer.Person.ID, PrincipalID: answer.Person.PrincipalID, PersonDisplayName: answer.Person.DisplayName}, nil
}
