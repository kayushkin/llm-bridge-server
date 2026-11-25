package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Every write and read of the peer tables runs on Postgres: ids keep their
// form, a second live mapping is refused, and the outbox, open-message and
// attachment queries find what they should.
func TestPeerRecordsRoundTripOnPostgres(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	invite := PeerConnectionInvite{PassphraseHash: "h1"}
	invite.CreatedBy = "principal_000001"
	invite.CreatedAt = now
	invite.ExpiresAt = now.Add(time.Hour)
	if err := s.InsertPeerConnectionInvite(&invite); err != nil {
		t.Fatal(err)
	}
	if invite.ID != "peer_connection_invite_000001" {
		t.Fatalf("invite id %s", invite.ID)
	}
	if _, err := s.CheckPeerConnectionInvite("h1", now); err != nil {
		t.Fatal(err)
	}
	connection := PeerConnection{InboundCredentialHash: "in", OutboundCredentialID: "cred"}
	connection.Transport = msg.PeerTransport("direct")
	connection.Role = msg.PeerConnectionRole("accepting")
	connection.CreatedBy = "principal_000001"
	connection.CreatedAt = now
	if err := s.RedeemPeerConnectionInvite("h1", &connection, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckPeerConnectionInvite("h1", now); err == nil {
		t.Fatal("redeemed twice")
	}
	if written, err := s.RecordPeerConnectionOtherServerName(connection.ID, "other", now); err != nil || !written {
		t.Fatal(written, err)
	}
	if written, err := s.RecordPeerConnectionOtherServerName(connection.ID, "other", now); err != nil || written {
		t.Fatal(written, err)
	}
	if _, written, err := s.RecordPeerConnectionOtherServerID(connection.ID, "server_x"); err != nil || !written {
		t.Fatal(written, err)
	}
	if err := s.SetPeerConnectionPeerPrincipal(connection.ID, "principal_000002"); err != nil {
		t.Fatal(err)
	}
	grant := PeerGrant{TokenHash: "tok"}
	grant.ConnectionID = connection.ID
	grant.PeerPrincipalID = "principal_000002"
	grant.Level = "administrator"
	grant.CreatedBy = "principal_000001"
	grant.CreatedAt = now
	if err := s.InsertPeerGrant(&grant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LivePeerGrantOfConnection(connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokePeerGrant(grant.ID, "p", "r", now); err != nil {
		t.Fatal(err)
	}
	access := PeerAccess{ConnectionID: connection.ID, RemoteGrantID: "g", RemotePeerPrincipalID: "p", Level: "run_sessions", CredentialID: "c", ReceivedAt: now}
	if _, err := s.InsertPeerAccess(&access); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndPeerAccess(connection.ID, "g", "done", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPeerAccessLevelGroup("messages", "principal_000003", "p", now); err != nil {
		t.Fatal(err)
	}
	removed, err := s.RemovePeerConnectionByOtherServer(connection.ID, "gone", now)
	if err != nil || !removed.RemovedByOtherServer {
		t.Fatal(removed, err)
	}
	if list, err := s.ListPeerConnections(); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}

	mapping := PeerUserMapping{PeerPrincipalID: "a", RemotePrincipalID: "b", LocalPrincipalID: "c", CreatedBy: "d"}
	if err := s.InsertPeerUserMapping(&mapping); err != nil {
		t.Fatal(err)
	}
	again := PeerUserMapping{PeerPrincipalID: "a", RemotePrincipalID: "b", LocalPrincipalID: "c", CreatedBy: "d"}
	if err := s.InsertPeerUserMapping(&again); err != ErrPeerUserMappingExists {
		t.Fatalf("second mapping: %v", err)
	}
	if list, err := s.ListPeerUserMappings("a"); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if _, err := s.RemovePeerUserMapping(mapping.ID, "d", "r"); err != nil {
		t.Fatal(err)
	}
	s.CreateSession(&Session{SessionID: "br_parent", Harness: "claude_code", State: "idle", PrincipalID: "c"})
	if err := s.RecordSessionPeerOrigin(SessionPeerOrigin{SessionID: "br_parent", PeerPrincipalID: "a", RemotePrincipalID: "b"}); err != nil {
		t.Fatal(err)
	}
	if copied, err := s.CopySessionPeerOrigin("br_parent", "br_child"); err != nil || !copied {
		t.Fatal(copied, err)
	}

	token := ServicePrincipalToken{PrincipalID: "principal_000004", Description: "x", CreatedBy: "p"}
	if err := s.InsertServicePrincipalToken(&token, "th"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchServicePrincipalToken(token.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeServicePrincipalToken(token.ID, "p", "r"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSessionStarter(SessionStarter{SessionID: "br_parent", PrincipalID: "x", RecordedBy: "y", Backfilled: true}); err != nil {
		t.Fatal(err)
	}
	if starter, ok, err := s.SessionStarterOf("br_parent"); err != nil || !ok || !starter.Backfilled {
		t.Fatal(starter, ok, err)
	}

	message := PeerMessage{ConnectionID: connection.ID, Direction: msg.PeerMessageDirection("outgoing"), Kind: msg.PeerMessageKind("request"), State: msg.PeerMessageState("queued")}
	if err := s.InsertPeerMessage(&message); err != nil {
		t.Fatal(err)
	}
	if due, err := s.QueuedPeerMessagesDue(connection.ID, now.Add(time.Minute)); err != nil || len(due) != 1 {
		t.Fatal(due, err)
	}
	if err := s.RecordPeerMessageAttemptFailed(message.ID, "x", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if due, err := s.QueuedPeerMessagesDue("", now.Add(time.Minute)); err != nil || len(due) != 0 {
		t.Fatal(due, err)
	}
	if open, err := s.OpenOutgoingPeerMessages(connection.ID); err != nil || len(open) != 1 {
		t.Fatal(open, err)
	}
	if err := s.RecordPeerMessageDelivered(message.ID, "peer_message_000009", now); err != nil {
		t.Fatal(err)
	}
	status := PeerMessage{ConnectionID: connection.ID, Direction: msg.PeerMessageDirection("incoming"), Kind: msg.PeerMessageKind("status"), State: msg.PeerMessageState("done"), InReplyTo: message.ID, RemoteMessageID: "r1"}
	if err := s.InsertPeerMessage(&status); err != nil {
		t.Fatal(err)
	}
	if open, err := s.OpenOutgoingPeerMessages(""); err != nil || len(open) != 0 {
		t.Fatal(open, err)
	}
	if list, err := s.ListPeerMessages(PeerMessageFilter{ConnectionID: connection.ID, Direction: "incoming", Limit: 5}); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if got, err := s.IncomingPeerMessageBySenderID(connection.ID, "r1"); err != nil || got.ID != status.ID {
		t.Fatal(got, err)
	}

	attachment := PeerMessageAttachment{Name: "a.patch", SHA256: "abc", Size: 3}
	if err := s.InsertStagedPeerAttachment(&attachment, "p"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimStagedPeerAttachments([]string{attachment.ID}, message.ID, "p"); err != nil {
		t.Fatal(err)
	}
	incoming := PeerMessageAttachment{MessageID: status.ID, RemoteAttachmentID: "x", Name: "b", SHA256: "d"}
	if err := s.InsertIncomingPeerAttachment(&incoming); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPeerAttachmentFetchFailed(incoming.ID, "no", now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if due, err := s.PeerAttachmentsToFetch(now); err != nil || len(due) != 0 {
		t.Fatal(due, err)
	}
	if err := s.RecordPeerAttachmentFetched(incoming.ID, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetPeerAttachment(incoming.ID); err != nil || got.State != msg.PeerMessageAttachmentState("fetched") || got.Attempts != 1 {
		t.Fatal(got, err)
	}
}
