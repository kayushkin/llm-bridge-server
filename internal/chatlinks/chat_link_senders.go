package chatlinks

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Who sent a chat link message, by id. Each inbound message carries the
// platform's id of its sender; before it is delivered, people-store (in this
// process) is asked which person in the session owner's records holds that
// account and what principal the person has. The
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
func (s *Service) resolveChatLinkSenders(ctx context.Context, link store.ChatLink, sessionPrincipalID string, messages []store.ChatLinkMessage) ([]store.ChatLinkMessage, error) {
	resolved := make([]store.ChatLinkMessage, len(messages))
	for index, message := range messages {
		resolved[index] = message
		if !message.SenderResolvedAt.IsZero() {
			continue
		}
		sender, err := s.ChatLinks.lookUpSender(ctx, sessionPrincipalID, link.Platform, message.SenderPlatformUserID)
		now := s.ChatLinks.Now()
		if err != nil {
			reason := fmt.Sprintf("could not read who sent %s from people-store: %v", message.ID, err)
			log.Printf("[chat-links] ERROR: %s: %s", link.ID, reason)
			ids := make([]string, len(messages))
			for index, message := range messages {
				ids[index] = message.ID
			}
			if recordErr := s.Store.RecordChatLinkDeliveryAttemptFailed(ids, reason, now); recordErr != nil {
				log.Printf("[chat-links] ERROR: %s: record the failed delivery: %v", link.ID, recordErr)
			}
			return nil, err
		}
		if err := s.Store.RecordChatLinkMessageSender(message.ID, sender, now); err != nil {
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

// chatLinkSenderDirectory is people-store as chat links read it: the person
// in one owner's records who holds a platform account, with that person's
// principal. An account nobody there holds is an empty sender and no error.
type chatLinkSenderDirectory interface {
	SenderOfAccount(ctx context.Context, ownerPrincipalID, kind, value string) (store.ChatLinkMessageSender, error)
}

// lookUpSender asks people-store who owns a platform account, in the records
// of the principal the session runs as: whose message it is is that person's
// own reading of their contacts, and a record in someone else's people cannot
// make a message read as the session owner's. An account nobody owns, or one
// with no id or no identity kind, is an empty sender and no error;
// people-store failing to answer is an error, and so is a session that runs as
// no principal, since then there are no records to read.
func (c *ChatLinks) lookUpSender(ctx context.Context, sessionPrincipalID, platform, platformUserID string) (store.ChatLinkMessageSender, error) {
	kind := chatLinkIdentityKinds[platform]
	if kind == "" || platformUserID == "" {
		return store.ChatLinkMessageSender{}, nil
	}
	if sessionPrincipalID == "" {
		return store.ChatLinkMessageSender{}, errors.New("the session runs as no principal, so there are no people records to read the sender from")
	}
	return c.Senders.SenderOfAccount(ctx, sessionPrincipalID, kind, platformUserID)
}
