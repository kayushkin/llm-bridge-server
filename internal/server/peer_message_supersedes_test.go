package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func sendSupersedingRequest(t *testing.T, from *peerTestServer, connectionID, title string, supersedes ...string) msg.PeerMessage {
	t.Helper()
	return decodeAnswer[msg.PeerMessage](t, from.requestAsAdministrator(t, "POST", "/peer-connections/"+connectionID+"/messages",
		msg.PeerMessageCreateRequest{Kind: msg.PeerMessageKindRequest, Body: msg.PeerMessageBody{Title: title, Prompt: "p"}, Supersedes: supersedes}),
		http.StatusCreated, "send "+title)
}

func openMessageIDs(t *testing.T, server *peerTestServer, connectionID string) []string {
	t.Helper()
	var opened []string
	for _, message := range peerMessagesOn(t, server, "open=true&connection_id="+connectionID) {
		opened = append(opened, message.ID)
	}
	return opened
}

// TestASupersedingRequestDeclinesTheWaitingOneOnBothSides sends a request,
// then one that replaces it: the receiver declines the first by itself and
// says so, and only the second is still open on the sender.
func TestASupersedingRequestDeclinesTheWaitingOneOnBothSides(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	grantPeerLevel(t, accepting, acceptingID, "messages")
	first := sendSupersedingRequest(t, initiating, initiatingID, "update to a1")
	if got := openMessageIDs(t, initiating, initiatingID); !slices.Equal(got, []string{first.ID}) {
		t.Fatalf("open after the first send = %v, want [%s]", got, first.ID)
	}
	second := sendSupersedingRequest(t, initiating, initiatingID, "update to b2", first.ID)
	if !slices.Equal(second.Supersedes, []string{first.ID}) {
		t.Fatalf("second = %+v, want it to record that it replaces %s", second, first.ID)
	}

	replaced := waitForPeerMessage(t, accepting, "direction=incoming&kind=request", func(m msg.PeerMessage) bool { return m.ID == first.RemoteMessageID })
	if replaced.State != msg.PeerMessageStateDeclined || replaced.SupersededBy != second.RemoteMessageID || !strings.Contains(replaced.DecisionReason, "superseded by "+second.RemoteMessageID) {
		t.Fatalf("the replaced request on the receiver = %+v, want declined and superseded by %s", replaced, second.RemoteMessageID)
	}
	replacing := waitForPeerMessage(t, accepting, "direction=incoming&kind=request", func(m msg.PeerMessage) bool { return m.ID == second.RemoteMessageID })
	if replacing.State != msg.PeerMessageStatePending || !slices.Equal(replacing.Supersedes, []string{first.RemoteMessageID}) {
		t.Fatalf("the replacing request on the receiver = %+v, want pending and naming %s in the receiver's ids", replacing, first.RemoteMessageID)
	}

	status := waitForPeerMessage(t, initiating, "direction=incoming&kind=status", func(m msg.PeerMessage) bool { return m.InReplyTo == first.ID })
	if status.Body.Result != msg.PeerMessageStatusDeclined {
		t.Fatalf("status for the replaced request = %+v, want declined", status)
	}
	if got := openMessageIDs(t, initiating, initiatingID); !slices.Equal(got, []string{second.ID}) {
		t.Fatalf("open after superseding = %v, want only [%s]", got, second.ID)
	}
}

// TestAMessageStillQueuedIsNeverSentOnceReplaced queues a request while the
// socket is closed, replaces it, and checks only the replacement arrives.
func TestAMessageStillQueuedIsNeverSentOnceReplaced(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	grantPeerLevel(t, accepting, acceptingID, "messages")
	initiating.server.peerSockets.stop(initiatingID)
	deadline := time.Now().Add(5 * time.Second)
	for initiating.server.peerSockets.socketOf(initiatingID) != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	queued := sendSupersedingRequest(t, initiating, initiatingID, "old")
	replacement := sendSupersedingRequest(t, initiating, initiatingID, "new", queued.ID)
	cancelled := decodeAnswer[msg.PeerMessage](t, initiating.requestAsAdministrator(t, "GET", "/peer-messages/"+queued.ID, nil), http.StatusOK, "read the queued one")
	if cancelled.State != msg.PeerMessageStateFailed || cancelled.SupersededBy != replacement.ID || cancelled.RemoteMessageID != "" {
		t.Fatalf("the queued one = %+v, want failed, superseded by %s and never sent", cancelled, replacement.ID)
	}
	initiating.server.startPeerDialer(mustPeerConnection(t, initiating, initiatingID))
	t.Cleanup(func() { initiating.server.peerSockets.stop(initiatingID) })
	delivered := waitForPeerMessage(t, initiating, "direction=outgoing", func(m msg.PeerMessage) bool {
		return m.ID == replacement.ID && m.State == msg.PeerMessageStateDelivered
	})
	received := peerMessagesOn(t, accepting, "direction=incoming&kind=request")
	if len(received) != 1 || received[0].ID != delivered.RemoteMessageID || len(received[0].Supersedes) != 0 {
		t.Fatalf("the receiver holds %+v, want only the replacement, naming nothing it never got", received)
	}
}

// TestSupersedesNamesOnlyThisServersOwnRequestsNoticesAndPatches checks the
// refusals: a status replaces nothing, a received message or an unknown id
// cannot be replaced, and a message is replaced only once.
func TestSupersedesNamesOnlyThisServersOwnRequestsNoticesAndPatches(t *testing.T) {
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t)
	grantPeerLevel(t, accepting, acceptingID, "messages")
	first := sendSupersedingRequest(t, initiating, initiatingID, "first")
	refused := func(name string, request msg.PeerMessageCreateRequest) {
		t.Helper()
		answer := initiating.requestAsAdministrator(t, "POST", "/peer-connections/"+initiatingID+"/messages", request)
		if answer.Code != http.StatusBadRequest || !strings.Contains(answer.Body.String(), "invalid_supersedes") {
			t.Fatalf("%s = %d %s, want 400 invalid_supersedes", name, answer.Code, answer.Body)
		}
	}
	request := msg.PeerMessageBody{Title: "t", Prompt: "p"}
	refused("a status superseding", msg.PeerMessageCreateRequest{Kind: msg.PeerMessageKindStatus, Body: msg.PeerMessageBody{Result: msg.PeerMessageStatusSucceeded, Detail: "d"}, Supersedes: []string{first.ID}})
	refused("an unknown id", msg.PeerMessageCreateRequest{Kind: msg.PeerMessageKindRequest, Body: request, Supersedes: []string{"peer_message_009999"}})
	sendSupersedingRequest(t, initiating, initiatingID, "second", first.ID)
	refused("a message already replaced", msg.PeerMessageCreateRequest{Kind: msg.PeerMessageKindRequest, Body: request, Supersedes: []string{first.ID}})
	if answer := initiating.requestAsAdministrator(t, "GET", "/peer-messages?open=true&direction=incoming", nil); answer.Code != http.StatusBadRequest {
		t.Fatalf("open=true with a direction = %d, want 400", answer.Code)
	}
}
