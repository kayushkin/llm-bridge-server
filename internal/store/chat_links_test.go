package store

import (
	"errors"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestChatLinkLifecycle(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	link, err := s.CreateChatLink(ChatLink{SessionID: "br_1", AccountID: "message_account_000002", Platform: "discord",
		ConversationID: "dm-1", PersonDisplayName: "Carol", CreatedByPrincipalID: "principal_000001", CreatedAt: now})
	if err != nil || link.ID != "chat_link_000001" {
		t.Fatalf("create: %+v %v", link, err)
	}
	if _, err := s.CreateChatLink(ChatLink{SessionID: "br_2", AccountID: "message_account_000002", Platform: "discord", ConversationID: "dm-1", CreatedAt: now}); !errors.Is(err, ErrChatLinkConversationTaken) {
		t.Fatalf("second active link to one conversation: %v", err)
	}
	found, ok, err := s.ActiveChatLinkFor("message_account_000002", "discord", "dm-1")
	if err != nil || !ok || found.ID != link.ID {
		t.Fatalf("active for: %+v %v %v", found, ok, err)
	}

	inserted, err := s.AddInboundChatLinkMessage(InboundChatLinkMessage{LinkID: link.ID, BusEventID: "discord:10", Body: "hi", Attachments: []msg.ChatLinkAttachment{{URL: "https://cdn.example/a.png", Filename: "a.png"}}, PlatformMessageID: "10", SenderPlatformUserID: "900", SenderPlatformDisplayName: "carol", At: now})
	if err != nil || !inserted {
		t.Fatalf("inbound: %v %v", inserted, err)
	}
	if again, err := s.AddInboundChatLinkMessage(InboundChatLinkMessage{LinkID: link.ID, BusEventID: "discord:10", Body: "hi", PlatformMessageID: "10", At: now}); err != nil || again {
		t.Fatalf("redelivered bus event kept twice: %v %v", again, err)
	}
	if _, err := s.AddInboundChatLinkMessage(InboundChatLinkMessage{LinkID: link.ID, BusEventID: "discord:11", Body: "there", PlatformMessageID: "11", At: now}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ChatLinkMessagesInState(link.ID, msg.ChatLinkInbound, msg.ChatLinkMessagePending)
	if err != nil || len(pending) != 2 || len(pending[0].Attachments) != 1 || pending[0].PlatformMessageIDs[0] != "10" {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if pending[0].SenderPlatformUserID != "900" || pending[0].SenderPlatformDisplayName != "carol" || !pending[0].SenderResolvedAt.IsZero() {
		t.Fatalf("sender as kept: %+v", pending[0])
	}
	if err := s.RecordChatLinkMessageSender(pending[0].ID, ChatLinkMessageSender{PersonID: "person_000007", PrincipalID: "principal_000009", PersonDisplayName: "Carol"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChatLinkMessageSender(pending[1].ID, ChatLinkMessageSender{}, now); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.ChatLinkMessagesInState(link.ID, msg.ChatLinkInbound, msg.ChatLinkMessagePending)
	if pending[0].SenderPersonID != "person_000007" || pending[0].SenderPrincipalID != "principal_000009" || pending[0].SenderPersonDisplayName != "Carol" || !pending[0].SenderResolvedAt.Equal(now) {
		t.Fatalf("sender after reading: %+v", pending[0])
	}
	if pending[1].SenderPersonID != "" || pending[1].SenderResolvedAt.IsZero() {
		t.Fatalf("a sender nobody owns: %+v", pending[1])
	}
	if err := s.addChatLinkMessageSenderColumns(); err != nil {
		t.Fatalf("adding the columns again: %v", err)
	}
	if err := s.MarkChatLinkMessagesInSession([]string{pending[0].ID, pending[1].ID}, "chat-link:1", "msg_x", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkChatLinkMessagesInSession([]string{pending[0].ID}, "chat-link:2", "msg_y", now); err == nil {
		t.Fatal("a message went into the session twice")
	}
	delivered, _ := s.ChatLinkMessagesOfDelivery("chat-link:1")
	if len(delivered) != 2 || delivered[0].State != msg.ChatLinkMessageInSession || delivered[0].Attempts != 1 {
		t.Fatalf("in session: %+v", delivered)
	}

	reply, err := s.AddOutboundChatLinkMessage(link.ID, "chat-link:1", "hello Carol", msg.ChatLinkMessagePending, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if same, _ := s.AddOutboundChatLinkMessage(link.ID, "chat-link:1", "again", msg.ChatLinkMessagePending, "", now); same.ID != reply.ID {
		t.Fatal("one delivery got two replies")
	}
	due, _ := s.DueOutboundChatLinkMessages(now)
	if len(due) != 1 {
		t.Fatalf("due: %+v", due)
	}
	if err := s.RecordChatLinkSendFailed(reply.ID, "multichat down", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueOutboundChatLinkMessages(now.Add(30 * time.Second)); len(due) != 0 {
		t.Fatal("retried before its time")
	}
	if due, _ := s.DueOutboundChatLinkMessages(now.Add(2 * time.Minute)); len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("due after wait: %+v", due)
	}
	if err := s.SetChatLinkMessageState(reply.ID, msg.ChatLinkMessageDelivered, "", []string{"900"}, now); err != nil {
		t.Fatal(err)
	}
	log, _ := s.ChatLinkMessages(link.ID)
	if len(log) != 3 || log[2].State != msg.ChatLinkMessageDelivered || log[2].PlatformMessageIDs[0] != "900" {
		t.Fatalf("log: %+v", log)
	}

	ended, err := s.EndChatLink("br_1", link.ID, now)
	if err != nil || ended.EndedAt.IsZero() {
		t.Fatalf("end: %+v %v", ended, err)
	}
	if _, ok, _ := s.ActiveChatLinkFor("message_account_000002", "discord", "dm-1"); ok {
		t.Fatal("an ended link is still active")
	}
	if _, err := s.CreateChatLink(ChatLink{SessionID: "br_2", AccountID: "message_account_000002", Platform: "discord", ConversationID: "dm-1", CreatedAt: now}); err != nil {
		t.Fatalf("a new link after the old one ended: %v", err)
	}
	if _, err := s.GetChatLink("br_2", link.ID); !errors.Is(err, ErrChatLinkNotFound) {
		t.Fatalf("another session's link: %v", err)
	}
}
