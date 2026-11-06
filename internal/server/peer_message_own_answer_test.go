package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// approvedRequestOnAccepting sends a request from initiating, approves it on
// accepting and returns the sender's copy and the receiver's approved copy.
func approvedRequestOnAccepting(t *testing.T, accepting, initiating *peerTestServer, acceptingID, initiatingID string) (msg.PeerMessage, msg.PeerMessage) {
	t.Helper()
	grantPeerLevel(t, accepting, acceptingID, "messages")
	// "hang" makes the mock harness hold the session's turn open, so the
	// only turn end is the one the test reports; otherwise the mock's own
	// reply can reach the sender first.
	sent := sendPeerMessageAsAdministrator(t, initiating, initiatingID, msg.PeerMessageKindRequest, msg.PeerMessageBody{Title: "t", Prompt: "do it, then hang"}, "")
	approved := decodeAnswer[msg.PeerMessage](t, accepting.requestAsAdministrator(t, "POST", "/peer-messages/"+sent.RemoteMessageID+"/approve",
		map[string]any{"instance_id": "inst_test"}), http.StatusOK, "approve")
	stopHeldPeerRequestSessionAtCleanup(t, accepting, approved.SessionID)
	return sent, approved
}

func statusesAnswering(t *testing.T, server *peerTestServer, id string) []msg.PeerMessage {
	t.Helper()
	var answering []msg.PeerMessage
	for _, message := range peerMessagesOn(t, server, "direction=incoming&kind=status") {
		if message.InReplyTo == id {
			answering = append(answering, message)
		}
	}
	return answering
}

// TestASessionsOwnFailedStatusAnswersTheRequestAndNoAutomaticOneFollows: the
// session reports a failure itself, the request is done with that status, and
// its turn end sends nothing more, so the sender never reads "succeeded".
func TestASessionsOwnFailedStatusAnswersTheRequestAndNoAutomaticOneFollows(t *testing.T) {
	buildMockHarnessOnPath(t)
	logStore := fakeLogStore(t).URL
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t, func(cfg *config.Config) { cfg.LogStoreURL = logStore })
	sent, approved := approvedRequestOnAccepting(t, accepting, initiating, acceptingID, initiatingID)

	own := decodeAnswer[msg.PeerMessage](t, accepting.requestAsAdministrator(t, "POST", "/peer-connections/"+acceptingID+"/messages",
		msg.PeerMessageCreateRequest{Kind: msg.PeerMessageKindStatus, InReplyTo: approved.ID,
			Body: msg.PeerMessageBody{Result: msg.PeerMessageStatusFailed, Detail: "the repo is not on this machine"}}), http.StatusCreated, "send the session's own status")
	done, err := accepting.server.store.GetPeerMessage(approved.ID)
	if err != nil || done.State != msg.PeerMessageStateDone || done.StatusMessageID != own.ID {
		t.Fatalf("the request after its own status = %+v, %v, want done, answered by %s", done, err, own.ID)
	}
	accepting.server.reportPeerRequestTurnEnd(approved.SessionID, &msg.Event{Result: &msg.ResultEvent{Text: "All done!"}}, msg.SessionIdle)

	waitForPeerMessage(t, initiating, "direction=incoming&kind=status", func(m msg.PeerMessage) bool { return m.InReplyTo == sent.ID })
	answers := statusesAnswering(t, initiating, sent.ID)
	if len(answers) != 1 || answers[0].Body.Result != msg.PeerMessageStatusFailed {
		t.Fatalf("the sender holds %+v answering %s, want only the session's own failed status", answers, sent.ID)
	}
}

// TestARequestWhoseSessionSaysNothingStillReportsItsReply keeps the old
// answer for a request: the final reply, as succeeded.
func TestARequestWhoseSessionSaysNothingStillReportsItsReply(t *testing.T) {
	buildMockHarnessOnPath(t)
	logStore := fakeLogStore(t).URL
	accepting, initiating, acceptingID, initiatingID := connectedPairByDialingOut(t, func(cfg *config.Config) { cfg.LogStoreURL = logStore })
	sent, approved := approvedRequestOnAccepting(t, accepting, initiating, acceptingID, initiatingID)
	frame := framedPeerRequestPrompt(approved, mustPeerConnection(t, accepting, acceptingID), approved.Body)
	if !strings.Contains(frame, "bridge-peer status "+acceptingID+" --in-reply-to "+approved.ID+" --result failed") {
		t.Fatalf("the request frame does not say how to report a failure:\n%s", frame)
	}
	accepting.server.reportPeerRequestTurnEnd(approved.SessionID, &msg.Event{Result: &msg.ResultEvent{Text: "hostname is laptop"}}, msg.SessionIdle)
	status := waitForPeerMessage(t, initiating, "direction=incoming&kind=status", func(m msg.PeerMessage) bool { return m.InReplyTo == sent.ID })
	if status.Body.Result != msg.PeerMessageStatusSucceeded || status.Body.Detail != "hostname is laptop" {
		t.Fatalf("status = %+v, want succeeded with the reply", status)
	}
}

func TestAutomaticTurnEndStatus(t *testing.T) {
	reply := &msg.Event{Result: &msg.ResultEvent{Text: "it worked"}}
	cases := []struct {
		name   string
		kind   msg.PeerMessageKind
		event  *msg.Event
		state  msg.SessionState
		result msg.PeerMessageStatusResult
	}{
		{"a request that answered", msg.PeerMessageKindRequest, reply, msg.SessionIdle, msg.PeerMessageStatusSucceeded},
		{"a request whose turn errored", msg.PeerMessageKindRequest, &msg.Event{Result: &msg.ResultEvent{Text: "x", IsError: true}}, msg.SessionIdle, msg.PeerMessageStatusFailed},
		{"a request whose session aborted", msg.PeerMessageKindRequest, nil, msg.SessionAborted, msg.PeerMessageStatusFailed},
		{"a patch whose session said nothing", msg.PeerMessageKindPatch, reply, msg.SessionIdle, msg.PeerMessageStatusFailed},
		{"a patch with no reply at all", msg.PeerMessageKindPatch, nil, msg.SessionIdle, msg.PeerMessageStatusFailed},
	}
	for _, c := range cases {
		if got := automaticTurnEndStatus(c.kind, c.event, c.state); got.Result != c.result {
			t.Errorf("%s: %s, want %s", c.name, got.Result, c.result)
		}
	}
}
