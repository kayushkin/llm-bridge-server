package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/bundlestore"
	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/grantstore"
	"github.com/kayushkin/kanban-store/internal/llmbridge"
	"github.com/kayushkin/kanban-store/internal/messaging"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/multichat"
	"github.com/kayushkin/kanban-store/internal/noteboard"
	"github.com/kayushkin/kanban-store/internal/principalstore"
)

// ============================ Harness ============================

// messageTriggerSender stands in for multichat: it records what it was asked
// to send, or refuses every send with fail.
type messageTriggerSender struct {
	mu   sync.Mutex
	sent []sentTriggerMessage
	fail error
}

type sentTriggerMessage struct {
	RecipientUserID string
	Body            string
}

func (s *messageTriggerSender) SendDirectMessage(_ context.Context, recipientUserID, body string) (multichat.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return multichat.SendResult{}, s.fail
	}
	s.sent = append(s.sent, sentTriggerMessage{RecipientUserID: recipientUserID, Body: body})
	return multichat.SendResult{RoomID: "!room:test", EventID: "$event" + itoa(len(s.sent))}, nil
}

func (s *messageTriggerSender) messages() []sentTriggerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentTriggerMessage(nil), s.sent...)
}

// setupWithMessageSender is the usual harness with a message sender attached.
// Passing a nil sender leaves delivery unconfigured, as MULTICHAT_URL unset does.
func setupWithMessageSender(t *testing.T, sender messaging.Sender) (http.Handler, *db.Store, *noteboard.Client) {
	t.Helper()
	principals := httptest.NewServer(newFakePrincipalStore().handler())
	t.Cleanup(principals.Close)
	bridge := httptest.NewServer(newFakeLLMBridgeServer().handler())
	t.Cleanup(bridge.Close)
	bundles := httptest.NewServer(newFakeBundleStore().handler())
	t.Cleanup(bundles.Close)
	notes := httptest.NewServer(newFakeNoteboard().handler())
	t.Cleanup(notes.Close)
	store, err := db.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	nb := noteboard.New(notes.URL)
	a := api.New(store, nb, principalstore.New(principals.URL), llmbridge.New(bridge.URL), bundlestore.New(bundles.URL), settingsRegistryForTests(t))
	a.SetReferenceStore(fakeReferenceStore(t))
	// Cleanups run last-registered first, so this waits for trigger
	// dispatches before the store above is closed.
	t.Cleanup(a.WaitForMessageTriggers)
	a.SetPrincipalEnforcement(api.PrincipalEnforcement{ServiceToken: testServiceToken, Grants: grantstore.New("http://127.0.0.1:1", "")})
	if sender != nil {
		a.SetMessageSender(sender)
	}
	return a.Handler(), store, nb
}

func putTestLadder(t *testing.T, h http.Handler, boardID string) {
	t.Helper()
	w := do(t, h, "PUT", "/api/boards/"+boardID+"/priority-levels", model.SetPriorityLadderRequest{Levels: []model.BoardPriorityLevel{
		{PriorityValue: 5, Label: "P0"},
		{PriorityValue: 3, Label: "P2"},
	}})
	if w.Code != 200 {
		t.Fatalf("put ladder: %d %s", w.Code, w.Body.String())
	}
}

func mkTriggerCard(t *testing.T, h http.Handler, boardID, columnID, title string, priority int) string {
	t.Helper()
	req := model.CreateCardRequest{Title: title, ColumnID: columnID}
	if priority != 0 {
		req.Priority = &priority
	}
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", req)
	if w.Code != 201 {
		t.Fatalf("create card: %d %s", w.Code, w.Body.String())
	}
	var view model.CardView
	decodeSuccessfulResponse(t, w, &view)
	return view.Placement.CardID
}

func mkMessageTrigger(t *testing.T, h http.Handler, boardID string, body map[string]any) model.MessageTrigger {
	t.Helper()
	w := do(t, h, "POST", "/api/boards/"+boardID+"/message-triggers", body)
	if w.Code != 201 {
		t.Fatalf("create trigger: %d %s", w.Code, w.Body.String())
	}
	var trigger model.MessageTrigger
	decodeSuccessfulResponse(t, w, &trigger)
	return trigger
}

// waitForMessageDeliveries polls until the board has exactly count delivery
// rows and none is still pending. More rows than count fails at once.
func waitForMessageDeliveries(t *testing.T, h http.Handler, boardID string, count int) []model.MessageDelivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := do(t, h, "GET", "/api/boards/"+boardID+"/message-deliveries", nil)
		if w.Code != 200 {
			t.Fatalf("list deliveries: %d %s", w.Code, w.Body.String())
		}
		var deliveries []model.MessageDelivery
		decodeSuccessfulResponse(t, w, &deliveries)
		if len(deliveries) > count {
			t.Fatalf("expected %d deliveries, got %d: %+v", count, len(deliveries), deliveries)
		}
		settled := len(deliveries) == count
		for _, delivery := range deliveries {
			if delivery.Status == model.DeliveryPending {
				settled = false
			}
		}
		if settled {
			return deliveries
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d settled deliveries; have %+v", count, deliveries)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertMessageDeliveriesStay gives any stray dispatch time to land, then
// checks the count did not grow.
func assertMessageDeliveriesStay(t *testing.T, h http.Handler, boardID string, count int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	waitForMessageDeliveries(t, h, boardID, count)
}

// ============================ Writes ============================

func TestMessageTriggerWriteIsCheckedAgainstTheBoard(t *testing.T) {
	h, _, _ := setupWithMessageSender(t, nil)
	board := mkBoard(t, h, "Ops")
	other := mkBoard(t, h, "Other")
	todo := mkColumn(t, h, board, "Todo", "")
	otherColumn := mkColumn(t, h, other, "Todo", "")
	putTestLadder(t, h, board)

	valid := func() map[string]any {
		return map[string]any{
			"name": "P0 created", "event_kind": "card_created", "priority_value": 5,
			"recipient_user_id": "@whatsapp_15551234567:chat.test", "message_template": "{{.Title}}",
		}
	}
	refusals := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"unknown event kind", func(b map[string]any) { b["event_kind"] = "card_deleted" }, "event_kind"},
		{"column on a kind that is not a move", func(b map[string]any) { b["to_column_id"] = todo }, "to_column_id only applies"},
		{"column of another board", func(b map[string]any) { b["event_kind"] = "card_moved"; b["to_column_id"] = otherColumn }, "not of this board"},
		{"priority that is not a rung", func(b map[string]any) { b["priority_value"] = 4 }, "not a rung"},
		{"recipient that is not a puppet id", func(b map[string]any) { b["recipient_user_id"] = "Alvaro" }, "recipient_user_id"},
		{"template that does not parse", func(b map[string]any) { b["message_template"] = "{{.Title" }, "message_template"},
		{"missing name", func(b map[string]any) { delete(b, "name") }, "name is required"},
		{"unknown field", func(b map[string]any) { b["recipient"] = "x" }, "unknown field"},
	}
	for _, refusal := range refusals {
		body := valid()
		refusal.mutate(body)
		w := do(t, h, "POST", "/api/boards/"+board+"/message-triggers", body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), refusal.want) {
			t.Errorf("%s: expected 400 containing %q, got %d %s", refusal.name, refusal.want, w.Code, w.Body.String())
		}
	}

	if w := do(t, h, "POST", "/api/boards/no-such-board/message-triggers", valid()); w.Code != 404 {
		t.Errorf("unknown board: expected 404, got %d %s", w.Code, w.Body.String())
	}

	created := mkMessageTrigger(t, h, board, valid())
	if !created.Enabled || created.PriorityLabel != "P0" || created.BoardID != board {
		t.Fatalf("created trigger: %+v", created)
	}

	w := do(t, h, "PATCH", "/api/message-triggers/"+created.ID, map[string]any{"enabled": false})
	var patched model.MessageTrigger
	decodeSuccessfulResponse(t, w, &patched)
	if w.Code != 200 || patched.Enabled || patched.Name != "P0 created" || patched.PriorityValue == nil {
		t.Fatalf("patch enabled=false: %d %+v", w.Code, patched)
	}
	w = do(t, h, "PATCH", "/api/message-triggers/"+created.ID, map[string]any{"clear_priority": true})
	// A fresh struct: a cleared priority is absent from the body, and decoding
	// into the previous response would leave its old value in place.
	var cleared model.MessageTrigger
	decodeSuccessfulResponse(t, w, &cleared)
	if w.Code != 200 || cleared.PriorityValue != nil || cleared.PriorityLabel != "" || cleared.Enabled {
		t.Fatalf("patch clear_priority: %d %+v", w.Code, cleared)
	}
	if w := do(t, h, "PATCH", "/api/message-triggers/"+created.ID, map[string]any{"to_column_id": todo}); w.Code != 400 {
		t.Errorf("patch that leaves the trigger invalid: expected 400, got %d %s", w.Code, w.Body.String())
	}

	if w := do(t, h, "DELETE", "/api/message-triggers/"+created.ID, nil); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, "GET", "/api/message-triggers/"+created.ID, nil); w.Code != 404 {
		t.Errorf("get after delete: expected 404, got %d", w.Code)
	}

	w = do(t, h, "GET", "/api/message-trigger-options", nil)
	var options struct {
		EventKinds         []string `json:"event_kinds"`
		TemplateFields     []string `json:"template_fields"`
		DeliveryConfigured bool     `json:"delivery_configured"`
	}
	decodeSuccessfulResponse(t, w, &options)
	if options.DeliveryConfigured || !contains(options.EventKinds, "card_created") || !contains(options.TemplateFields, "Title") {
		t.Errorf("options: %+v", options)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ============================ Delivery ============================

func TestP0CardCreatedSendsTheRenderedMessageToTheRecipient(t *testing.T) {
	sender := &messageTriggerSender{}
	h, _, _ := setupWithMessageSender(t, sender)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	putTestLadder(t, h, board)
	trigger := mkMessageTrigger(t, h, board, map[string]any{
		"name": "P0 created", "event_kind": "card_created", "priority_value": 5,
		"recipient_user_id": "@whatsapp_15551234567:chat.test",
		"message_template":  "{{.PriorityLabel}} on {{.BoardName}}: {{.Title}} ({{.ColumnName}})",
	})

	mkTriggerCard(t, h, board, todo, "Typo on the pricing page", 3)
	cardID := mkTriggerCard(t, h, board, todo, "Checkout is down", 5)

	deliveries := waitForMessageDeliveries(t, h, board, 1)
	delivery := deliveries[0]
	if delivery.Status != model.DeliverySent || delivery.CardID != cardID || delivery.TriggerID != trigger.ID ||
		delivery.RenderedMessage != "P0 on Ops: Checkout is down (Todo)" || delivery.RoomID != "!room:test" {
		t.Fatalf("delivery: %+v", delivery)
	}
	sent := sender.messages()
	if len(sent) != 1 || sent[0].RecipientUserID != "@whatsapp_15551234567:chat.test" || sent[0].Body != "P0 on Ops: Checkout is down (Todo)" {
		t.Fatalf("sent: %+v", sent)
	}
	assertMessageDeliveriesStay(t, h, board, 1)
}

func TestCardMovedTriggerFiresOnlyForMovesIntoItsColumn(t *testing.T) {
	sender := &messageTriggerSender{}
	h, _, _ := setupWithMessageSender(t, sender)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	doing := mkColumn(t, h, board, "Doing", "")
	done := mkColumn(t, h, board, "Done", "")
	mkMessageTrigger(t, h, board, map[string]any{
		"name": "Landed in Done", "event_kind": "card_moved", "to_column_id": done,
		"recipient_user_id": "@telegram_42:chat.test", "message_template": "{{.Title}}: {{.FromColumnName}} → {{.ColumnName}}",
	})
	cardID := mkTriggerCard(t, h, board, todo, "Rotate the keys", 0)

	for _, column := range []string{doing, done} {
		if w := do(t, h, "POST", "/api/cards/"+cardID+"/move", model.MoveCardRequest{BoardID: board, ColumnID: column}); w.Code != 200 {
			t.Fatalf("move: %d %s", w.Code, w.Body.String())
		}
	}
	deliveries := waitForMessageDeliveries(t, h, board, 1)
	if deliveries[0].RenderedMessage != "Rotate the keys: Doing → Done" {
		t.Fatalf("delivery: %+v", deliveries[0])
	}
	assertMessageDeliveriesStay(t, h, board, 1)
}

func TestCardCompletedFiresTheBoardsTrigger(t *testing.T) {
	sender := &messageTriggerSender{}
	h, _, _ := setupWithMessageSender(t, sender)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	mkMessageTrigger(t, h, board, map[string]any{
		"name": "Resolved", "event_kind": "card_completed",
		"recipient_user_id": "@signal_1:chat.test", "message_template": "Resolved: {{.Title}} ({{.Summary}}, was in {{.ColumnName}})",
	})
	cardID := mkTriggerCard(t, h, board, todo, "Flaky deploy", 0)
	if w := do(t, h, "PATCH", "/api/cards/"+cardID, map[string]any{"status": "done"}); w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	deliveries := waitForMessageDeliveries(t, h, board, 1)
	if deliveries[0].Status != model.DeliverySent || deliveries[0].RenderedMessage != "Resolved: Flaky deploy (done, was in Todo)" {
		t.Fatalf("delivery: %+v", deliveries[0])
	}
}

func TestTriggerWithoutMultichatRecordsWhatItWouldHaveSent(t *testing.T) {
	h, _, _ := setupWithMessageSender(t, nil)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	mkMessageTrigger(t, h, board, map[string]any{
		"name": "Any card", "event_kind": "card_created",
		"recipient_user_id": "@whatsapp_1:chat.test", "message_template": "New: {{.Title}}",
	})
	mkTriggerCard(t, h, board, todo, "Renew the certificate", 0)
	deliveries := waitForMessageDeliveries(t, h, board, 1)
	if deliveries[0].Status != model.DeliveryNotConfigured || deliveries[0].RenderedMessage != "New: Renew the certificate" ||
		!strings.Contains(deliveries[0].Error, "MULTICHAT_URL") {
		t.Fatalf("delivery: %+v", deliveries[0])
	}
}

func TestFailedSendAndBadTemplateAreRecordedAsFailed(t *testing.T) {
	sender := &messageTriggerSender{fail: errors.New("multichat POST /api/messages/send: 502 synapse is down")}
	h, _, _ := setupWithMessageSender(t, sender)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	mkMessageTrigger(t, h, board, map[string]any{
		"name": "Refused by multichat", "event_kind": "card_created",
		"recipient_user_id": "@whatsapp_1:chat.test", "message_template": "{{.Title}}",
	})
	mkMessageTrigger(t, h, board, map[string]any{
		"name": "Names a field that does not exist", "event_kind": "card_created",
		"recipient_user_id": "@whatsapp_2:chat.test", "message_template": "{{.Assignee}}",
	})
	mkTriggerCard(t, h, board, todo, "Anything", 0)
	deliveries := waitForMessageDeliveries(t, h, board, 2)
	var sawRefusal, sawTemplate bool
	for _, delivery := range deliveries {
		if delivery.Status != model.DeliveryFailed {
			t.Errorf("expected failed, got %+v", delivery)
		}
		sawRefusal = sawRefusal || strings.Contains(delivery.Error, "synapse is down")
		sawTemplate = sawTemplate || strings.Contains(delivery.Error, "render message_template")
	}
	if !sawRefusal || !sawTemplate {
		t.Fatalf("deliveries: %+v", deliveries)
	}
}

func TestDisabledTriggerDoesNotFire(t *testing.T) {
	sender := &messageTriggerSender{}
	h, _, _ := setupWithMessageSender(t, sender)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	disabled := mkMessageTrigger(t, h, board, map[string]any{
		"name": "Off", "event_kind": "card_created", "recipient_user_id": "@whatsapp_1:chat.test", "message_template": "off",
	})
	if w := do(t, h, "PATCH", "/api/message-triggers/"+disabled.ID, map[string]any{"enabled": false}); w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	enabled := mkMessageTrigger(t, h, board, map[string]any{
		"name": "On", "event_kind": "card_created", "recipient_user_id": "@whatsapp_2:chat.test", "message_template": "on",
	})
	mkTriggerCard(t, h, board, todo, "Anything", 0)
	deliveries := waitForMessageDeliveries(t, h, board, 1)
	if deliveries[0].TriggerID != enabled.ID {
		t.Fatalf("delivery came from %s, want %s", deliveries[0].TriggerID, enabled.ID)
	}
	assertMessageDeliveriesStay(t, h, board, 1)
}

func TestATriggerDeliversAnEventAtMostOnce(t *testing.T) {
	sender := &messageTriggerSender{}
	h, store, nb := setupWithMessageSender(t, sender)
	board := mkBoard(t, h, "Ops")
	todo := mkColumn(t, h, board, "Todo", "")
	mkMessageTrigger(t, h, board, map[string]any{
		"name": "Any card", "event_kind": "card_created", "recipient_user_id": "@whatsapp_1:chat.test", "message_template": "{{.Title}}",
	})
	cardID := mkTriggerCard(t, h, board, todo, "Once", 0)
	waitForMessageDeliveries(t, h, board, 1)

	events, err := store.ListCardEvents(cardID, board)
	if err != nil {
		t.Fatal(err)
	}
	var created *model.CardEvent
	for i := range events {
		if events[i].Kind == model.EventCardCreated {
			created = &events[i]
		}
	}
	if created == nil {
		t.Fatalf("no card_created event in %+v", events)
	}
	// The same event seen twice more — a retry, a second process — must not
	// reach the person again.
	dispatcher := messaging.NewDispatcher(store, nb)
	dispatcher.SetSender(sender)
	dispatcher.FireTriggersFor(*created)
	dispatcher.FireTriggersFor(*created)
	if sent := sender.messages(); len(sent) != 1 {
		t.Fatalf("expected 1 send, got %d: %+v", len(sent), sent)
	}
	waitForMessageDeliveries(t, h, board, 1)
}
