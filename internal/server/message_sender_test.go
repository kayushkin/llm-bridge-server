package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A message a person wrote while the server drained is sent after the drain
// with that person recorded as its sender, and their name beside the id.
func TestAHeldMessageIsSentInItsSendersName(t *testing.T) {
	srv, st, session := drainTestServer(t)
	srv.principalLookupCache = &principalLookupCache{
		principalsByID: map[string]cachedPrincipal{}, lifetime: time.Minute, now: time.Now,
		readFromUpstream: func(_ context.Context, principalID string) (principals.Principal, error) {
			return principals.Principal{ID: principalID, Kind: "human", DisplayName: "Priya Raman"}, nil
		},
	}
	srv.startRestartDrain(time.Hour, "test deploy")
	heldMessageID, held, err := srv.holdMessageIfRestartDraining(session.SessionID, msg.SendMessageRequest{Message: "from priya"}, "principal_000002", "send")
	if err != nil || !held {
		t.Fatalf("hold = %q %v %v", heldMessageID, held, err)
	}
	srv.endRestartDrain("test")
	waitForHeldMessagesToBeSent(t, st)
	waitForEvent(t, st, session.SessionID, msg.SystemSubtypeHeldMessageDelivered)

	sender := senderOfUserMessage(t, st, session.SessionID, "from priya")
	if sender.SenderPrincipalID != "principal_000002" || sender.SenderDisplayName != "Priya Raman" {
		t.Fatalf("the replayed message's sender = %q / %q, want principal_000002 / Priya Raman", sender.SenderPrincipalID, sender.SenderDisplayName)
	}
}

// A message from a caller acting as no principal records no sender.
func TestAMessageFromNoPrincipalHasNoSender(t *testing.T) {
	srv, st, session := drainTestServer(t)
	if status, _ := sendThroughRoute(t, srv, session.SessionID, "from a script"); status != http.StatusOK {
		t.Fatalf("send = %d", status)
	}
	sender := senderOfUserMessage(t, st, session.SessionID, "from a script")
	if sender.SenderPrincipalID != "" || sender.SenderDisplayName != "" {
		t.Fatalf("sender = %q / %q, want none", sender.SenderPrincipalID, sender.SenderDisplayName)
	}
}

// senderOfUserMessage is the server's own user_message event with this text.
func senderOfUserMessage(t *testing.T, st *store.Store, sessionID, text string) msg.Event {
	t.Helper()
	events, err := testSessionEventsOf(t, st).ListEventsSinceID(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range events {
		var event msg.Event
		if err := json.Unmarshal(row.Data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == msg.EventUserMessage && event.HarnessSessionID == "" && event.Result != nil && event.Result.Text == text {
			return event
		}
	}
	t.Fatalf("no user_message %q", text)
	return msg.Event{}
}
