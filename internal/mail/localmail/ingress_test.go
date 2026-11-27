package localmail

import (
	"context"
	"errors"
	"testing"
)

func testCredential(t *testing.T, store *Store, accounts ...string) IngressCredential {
	t.Helper()
	credential, token, err := store.CreateIngressCredential(context.Background(), "databricks", "test", accounts)
	if err != nil {
		t.Fatalf("CreateIngressCredential: %v", err)
	}
	found, err := store.IngressCredentialByToken(context.Background(), token)
	if err != nil || found.ID != credential.ID {
		t.Fatalf("token finds %+v err %v", found, err)
	}
	return credential
}

func delivery(deliveryID, sourceRevision, body string) Delivery {
	m := exported("PUSH-1")
	m.SourceRevision = sourceRevision
	m.BodyText = strPtr(body)
	return Delivery{DeliveryID: deliveryID, AccountID: "desk", Message: m}
}

func TestDeliverySemantics(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	credential := testCredential(t, store, "desk")
	deliver := func(d Delivery) DeliveryResult {
		t.Helper()
		result, err := store.Deliver(ctx, credential, d)
		if err != nil {
			t.Fatalf("Deliver %s: %v", d.DeliveryID, err)
		}
		return result
	}

	first := deliver(delivery("d1", "ck-1", "first"))
	if first.Status != DeliveryAccepted || first.Revision != 1 || first.ReceiptID == 0 {
		t.Fatalf("first %+v", first)
	}
	if again := deliver(delivery("d1", "ck-1", "first")); !again.Redelivered || again.Status != DeliveryAccepted || again.ReceiptID != first.ReceiptID {
		t.Fatalf("same delivery id %+v, want the first answer again", again)
	}
	if replay := deliver(delivery("d2", "ck-1", "first")); replay.Status != DeliveryReplay || replay.Revision != 1 {
		t.Fatalf("same source revision, same content %+v, want replay", replay)
	}
	conflict := deliver(delivery("d3", "ck-1", "changed under the same revision"))
	if conflict.Status != DeliveryConflict || conflict.Retry || conflict.Detail == "" {
		t.Fatalf("same source revision, different content %+v, want conflict", conflict)
	}
	body, _, err := store.Body(ctx, "desk", first.ID, BodyFormatText)
	if err != nil || body != "first" {
		t.Fatalf("a conflict changed the message: %q err %v", body, err)
	}
	second := deliver(delivery("d4", "ck-2", "second"))
	if second.Status != DeliveryAccepted || second.Revision != 2 || second.PreviousRevision != 1 {
		t.Fatalf("new source revision %+v, want revision 2", second)
	}
	if late := deliver(delivery("d5", "ck-1", "first")); late.Status != DeliveryReplay || late.Revision != 1 {
		t.Fatalf("a late copy of a replaced revision %+v, want replay of revision 1", late)
	}
	if late := deliver(delivery("d6", "ck-1", "not what ck-1 said")); late.Status != DeliveryConflict {
		t.Fatalf("a different copy of a replaced revision %+v, want conflict", late)
	}

	events, err := store.Events(ctx, 0, "", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 || events[0].Revision != 1 || events[1].Revision != 2 || events[0].Origin != EventOriginIngress || events[0].Kind != EventRevisionAvailable {
		t.Fatalf("events %+v, want one per stored revision and none for replays or conflicts", events)
	}
	receipts, err := store.Receipts(ctx, 0, "", 0)
	if err != nil || len(receipts) != 6 {
		t.Fatalf("receipts %d err %v, want one per distinct delivery id", len(receipts), err)
	}
	conflicts, err := store.Receipts(ctx, 0, DeliveryConflict, 0)
	if err != nil || len(conflicts) != 2 {
		t.Fatalf("conflict receipts %d err %v, want 2", len(conflicts), err)
	}
}

func TestAPurgedMessageIsRefusedFromEveryWayIn(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	credential := testCredential(t, store, "desk")
	first, err := store.Deliver(ctx, credential, delivery("p1", "ck-1", "keep me"))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if err := store.Purge(ctx, "desk", first.ID, ""); err == nil {
		t.Fatal("a purge without a reason was accepted")
	}
	if err := store.Purge(ctx, "desk", first.ID, "retention: 7 years"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := store.Metadata(ctx, "desk", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after purge: %v, want ErrNotFound", err)
	}
	again, err := store.Deliver(ctx, credential, delivery("p2", "ck-2", "back again"))
	if err != nil || again.Status != DeliveryPurged || again.Retry {
		t.Fatalf("delivery after purge %+v err %v, want purged and no retry", again, err)
	}
	m := exported("PUSH-1")
	_, _, _, err = store.Import(ctx, "desk", []ImportedMessage{m}, nil)
	var purged *PurgedError
	if !errors.As(err, &purged) {
		t.Fatalf("import after purge: %v, want PurgedError", err)
	}
	events, err := store.Events(ctx, 0, "desk", 0)
	if err != nil || len(events) != 2 || events[1].Kind != EventPurged {
		t.Fatalf("events %+v err %v, want available then purged", events, err)
	}
}

func TestEventsPageByCursorAndAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for i, account := range []string{"desk-a", "desk-b", "desk-a"} {
		m := exported("E-" + account)
		if i == 2 {
			m.Subject += " again" // changed, so a second revision and a second event
		}
		if _, _, _, err := store.Import(ctx, account, []ImportedMessage{m}, nil); err != nil {
			t.Fatalf("Import: %v", err)
		}
	}
	all, err := store.Events(ctx, 0, "", 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("events %+v err %v", all, err)
	}
	rest, err := store.Events(ctx, all[0].Seq, "", 0)
	if err != nil || len(rest) != 2 || rest[0].Seq != all[1].Seq {
		t.Fatalf("after %d: %+v", all[0].Seq, rest)
	}
	onlyA, err := store.Events(ctx, 0, "desk-a", 0)
	if err != nil || len(onlyA) != 2 || onlyA[0].Origin != EventOriginImport {
		t.Fatalf("desk-a events %+v err %v", onlyA, err)
	}
}

func TestARevokedCredentialFindsNothing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	credential, token, err := store.CreateIngressCredential(ctx, "databricks", "", []string{"desk"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.RevokeIngressCredential(ctx, credential.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.IngressCredentialByToken(ctx, token); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("revoked token: %v, want ErrCredentialNotFound", err)
	}
	if err := store.RevokeIngressCredential(ctx, "ingress_nope"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("unknown credential: %v", err)
	}
}
