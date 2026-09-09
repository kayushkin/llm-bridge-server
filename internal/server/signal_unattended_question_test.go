package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/mailstackclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// stubTriage points srv's question triage at a runner that answers every call
// with verdict, or fails with err when err is set. Returns the captured
// prompt so a test can assert what triage was told.
func stubTriage(t *testing.T, srv *Server, verdict map[string]any, err error) *string {
	t.Helper()
	var captured string
	runner := func(_ context.Context, req msg.OneShotRequest) ([]byte, error) {
		captured = req.Prompt
		if err != nil {
			return nil, err
		}
		parsed, mErr := json.Marshal(verdict)
		if mErr != nil {
			return nil, mErr
		}
		return json.Marshal(msg.OneShotResponse{Parsed: parsed, StopReason: "tool_use"})
	}
	srv.questionTriage = newQuestionTriage("claude-haiku-4-5", 5*time.Second, runner)
	return &captured
}

func seedWorkerSession(t *testing.T, st *store.Store, id, purpose string) *store.Session {
	t.Helper()
	sess := &store.Session{
		SessionID:  id,
		Harness:    "claude_code",
		InstanceID: "inst-1",
		State:      "tool_running",
		Purpose:    purpose,
		Type:       msg.SessionTypeAutonomous,
		Mode:       msg.SessionModeEvents,
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return sess
}

// postPrehookFull is postPrehook plus the updatedInput an allow carries.
func postPrehookFull(t *testing.T, srv *Server, bridgeID, body string) (decision, reason string, updatedInput map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/permission/cc-prehook/"+bridgeID, strings.NewReader(body))
	req.SetPathValue("bridge_id", bridgeID)
	rec := httptest.NewRecorder()
	srv.handleCCPermissionPrehook(rec, req)
	var got struct {
		HookSpecificOutput struct {
			PermissionDecision       string         `json:"permissionDecision"`
			PermissionDecisionReason string         `json:"permissionDecisionReason"`
			UpdatedInput             map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	return got.HookSpecificOutput.PermissionDecision, got.HookSpecificOutput.PermissionDecisionReason, got.HookSpecificOutput.UpdatedInput
}

const askWhichReading = `{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Should the count include discontinued products?","header":"The ticket says 'catalogue size' and the catalogue has 3 discontinued rows.","options":[{"label":"Include them"},{"label":"Exclude them"}]}]}}`

const askMayIProceed = `{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Shall I go ahead and open the pull request?","header":"Tests pass.","options":[{"label":"Yes"},{"label":"No"}]}]}}`

// cardStub is a kanban-store stand-in that knows one card with one email
// link, and records every event posted onto it.
type cardStub struct {
	mu     sync.Mutex
	events []map[string]any
}

func (c *cardStub) handler(cardID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cards"):
			_, _ = w.Write([]byte(`[{"card_id":"` + cardID + `","item":{"id":"` + cardID + `","title":"Add catalogue product count to /health","body":"Helena asks for a products count on /health."}}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/links"):
			_, _ = w.Write([]byte(`[{"entity_type":"repo","entity_ref":"/tmp/x"},{"entity_type":"email","entity_ref":"demo-work:lmf8c5178e02571eae"}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			var ev map[string]any
			_ = json.NewDecoder(r.Body).Decode(&ev)
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (c *cardStub) eventKinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.events))
	for _, ev := range c.events {
		kind, _ := ev["kind"].(string)
		out = append(out, kind)
	}
	return out
}

func stubMailstack(t *testing.T, srv *Server) {
	t.Helper()
	mail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("account") != "demo-work" {
			http.Error(w, "account query param required", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"lmf8c5178e02571eae","account_id":"demo-work","subject":"Catalogue size in /health","from":{"name":"Helena Ortiz","email":"helena@example.com"}}`))
	}))
	t.Cleanup(mail.Close)
	client, err := mailstackclient.New(mail.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	srv.mailstackClient = client
}

func TestSurfacingWorkerSignoffIsAnsweredWithoutAPerson(t *testing.T) {
	srv, st := testServer(t)
	seedWorkerSession(t, st, "demo-builder-1", msg.PurposeDispatcher)
	stubTriage(t, srv, map[string]any{"disposition": "signoff", "reason": "asks permission to do what the ticket asked"}, nil)

	decision, reason, updated := postPrehookFull(t, srv, "demo-builder-1", askMayIProceed)

	if decision != "allow" {
		t.Fatalf("decision = %q (%s), want allow: a sign-off is answered on the spot", decision, reason)
	}
	answers, _ := updated["answers"].(map[string]any)
	if got := answers["Shall I go ahead and open the pull request?"]; got != autoSignoffAnswer {
		t.Errorf("answer = %v, want the automatic sign-off answer", got)
	}
	if _, kept := updated["questions"]; !kept {
		t.Error("updatedInput dropped the original questions; the hook replaces the tool input wholesale")
	}
	if open := openSignals(t, st, "demo-builder-1"); len(open) != 0 {
		t.Errorf("open signals = %d, want 0: nobody was asked", len(open))
	}
	answered, err := st.ListSignals(store.SignalFilter{SessionID: "demo-builder-1", State: msg.SignalStateAnswered})
	if err != nil {
		t.Fatal(err)
	}
	if len(answered) != 1 || answered[0].Answer == nil || answered[0].Answer.Text != autoSignoffAnswer {
		t.Errorf("answered rows = %+v, want one row recording the automatic answer", answered)
	}
}

func TestSurfacingWorkerRealQuestionLandsOnTheCardWithADraftForTheCustomer(t *testing.T) {
	card := &cardStub{}
	srv, st := testServerWithKanban(t, card.handler("card-1"))
	stubMailstack(t, srv)
	seedWorkerSession(t, st, "demo-builder-2", msg.PurposeDispatcher)
	prompt := stubTriage(t, srv, map[string]any{
		"disposition": "needs_input",
		"reason":      "two readings of 'catalogue size'",
		"audience":    "customer",
		"customer_reply": map[string]any{
			"subject": "Re: Catalogue size in /health",
			"body":    "Hi Helena — should the count include the three discontinued products?",
		},
	}, nil)

	decision, reason, _ := postPrehookFull(t, srv, "demo-builder-2", askWhichReading)

	if decision != "deny" {
		t.Fatalf("decision = %q, want deny: the worker stops and the answer comes back as its next message", decision)
	}
	if !strings.Contains(reason, "recorded on the ticket") || !strings.Contains(reason, "next message") {
		t.Errorf("reason = %q, want it to tell the worker where the question went and how the answer returns", reason)
	}
	if !strings.Contains(*prompt, "Helena Ortiz <helena@example.com>") {
		t.Errorf("triage prompt did not name the requester from mailstack:\n%s", *prompt)
	}
	if !strings.Contains(*prompt, "Add catalogue product count") {
		t.Errorf("triage prompt did not carry the card's title:\n%s", *prompt)
	}

	open := openSignals(t, st, "demo-builder-2")
	if len(open) != 1 {
		t.Fatalf("open signals = %d, want 1", len(open))
	}
	sig := open[0]
	if sig.Surface != msg.SignalSurfaceKanban || sig.LinkedTodoID != "card-1" {
		t.Errorf("surface=%q linked_todo=%q, want kanban/card-1", sig.Surface, sig.LinkedTodoID)
	}
	if sig.RequestID == "" {
		t.Error("request_id empty; the questions of one ask must resolve together")
	}
	if srv.parkedAsks.isParked(sig.SessionID, sig.RequestID) {
		t.Error("the ask is parked; a surfaced question must not hold the worker's process open")
	}
	if sig.Audience != msg.SignalAudienceCustomer {
		t.Errorf("audience = %q, want customer", sig.Audience)
	}
	if sig.CustomerReplyDraft == nil {
		t.Fatal("no customer reply draft on a customer question")
	}
	if sig.CustomerReplyDraft.To != "helena@example.com" {
		t.Errorf("draft.to = %q, want the sender resolved from mailstack, never from the model", sig.CustomerReplyDraft.To)
	}
	if sig.CustomerReplyDraft.Subject != "Re: Catalogue size in /health" || !strings.Contains(sig.CustomerReplyDraft.Body, "discontinued") {
		t.Errorf("draft = %+v", sig.CustomerReplyDraft)
	}
	if kinds := card.eventKinds(); len(kinds) != 1 || kinds[0] != "waiting_started" {
		t.Errorf("card events = %v, want exactly [waiting_started]: the card's clock pauses while the question is open", kinds)
	}
}

func TestSurfacingWorkerQuestionIsSurfacedUntriagedWhenTriageFails(t *testing.T) {
	card := &cardStub{}
	srv, st := testServerWithKanban(t, card.handler("card-2"))
	seedWorkerSession(t, st, "demo-builder-3", msg.PurposeDispatcher)
	stubTriage(t, srv, nil, errors.New("oneshot instance is disabled"))

	decision, _, _ := postPrehookFull(t, srv, "demo-builder-3", askWhichReading)

	if decision != "deny" {
		t.Fatalf("decision = %q, want deny with the question surfaced", decision)
	}
	open := openSignals(t, st, "demo-builder-3")
	if len(open) != 1 {
		t.Fatalf("open signals = %d, want 1: triage filters, it does not gate", len(open))
	}
	if open[0].Audience != "" || open[0].CustomerReplyDraft != nil {
		t.Errorf("an untriaged question must carry no audience and no draft, got audience=%q draft=%+v", open[0].Audience, open[0].CustomerReplyDraft)
	}
}

func TestNonSurfacingAutonomousSessionIsStillDenied(t *testing.T) {
	srv, st := testServer(t)
	seedWorkerSession(t, st, "autoworker-1", msg.PurposeAutoworker)
	prompt := stubTriage(t, srv, map[string]any{"disposition": "signoff", "reason": "x"}, nil)

	decision, reason, _ := postPrehookFull(t, srv, "autoworker-1", askMayIProceed)

	if decision != "deny" || !strings.Contains(reason, "No human is attached") {
		t.Errorf("decision=%q reason=%q, want the plain unattended deny", decision, reason)
	}
	if *prompt != "" {
		t.Error("triage ran for a purpose that does not surface questions")
	}
	if n := len(openSignals(t, st, "autoworker-1")); n != 0 {
		t.Errorf("open signals = %d, want 0", n)
	}
}

func TestSkipClassifyReasonLetsASurfacingWorkerThrough(t *testing.T) {
	worker := &store.Session{Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeDispatcher}
	if got := skipClassifyReason(worker, "Should I drop the legacy column or keep it?", msg.SessionIdle); got != "" {
		t.Errorf("skip = %q, want none: a card worker's turn-end has a reader", got)
	}
	nightly := &store.Session{Type: msg.SessionTypeAutonomous, Purpose: msg.PurposeAutoworker}
	if got := skipClassifyReason(nightly, "Should I drop the legacy column or keep it?", msg.SessionIdle); got == "" {
		t.Error("an autoworker turn-end was classified; only surfacing purposes have a reader")
	}
}

func TestDerivedWorkerQuestionSurfacesWithItsAudience(t *testing.T) {
	card := &cardStub{}
	srv, st := testServerWithKanban(t, card.handler("card-3"))
	sess := seedWorkerSession(t, st, "demo-builder-4", msg.PurposeDispatcher)
	stubClassifier(t, srv, map[string]any{
		"kind":    "question",
		"title":   "Should the health check count discontinued products?",
		"options": []any{map[string]any{"label": "Yes"}, map[string]any{"label": "No"}},
	})
	stubTriage(t, srv, map[string]any{"disposition": "needs_input", "reason": "a real choice", "audience": "assignee"}, nil)

	srv.onTurnEnd(sess.SessionID, turnEndEvent("…so: should the health check count discontinued products?"), msg.SessionIdle)

	open := openSignals(t, st, sess.SessionID)
	if len(open) != 1 {
		t.Fatalf("open signals = %d, want 1", len(open))
	}
	if open[0].Source != msg.SignalSourceDerived || open[0].Audience != msg.SignalAudienceAssignee || open[0].LinkedTodoID != "card-3" {
		t.Errorf("signal = %+v", open[0])
	}
	if kinds := card.eventKinds(); len(kinds) != 1 || kinds[0] != "waiting_started" {
		t.Errorf("card events = %v, want [waiting_started]", kinds)
	}
}

func TestDerivedSignoffPastTheCapIsSurfacedNotAnswered(t *testing.T) {
	card := &cardStub{}
	srv, st := testServerWithKanban(t, card.handler("card-4"))
	sess := seedWorkerSession(t, st, "demo-builder-5", msg.PurposeDispatcher)
	now := time.Now().UTC()
	for i := 0; i < maxAutoContinuesPerSession; i++ {
		if err := st.CreateSignal(&msg.Signal{
			ID: "sig_prior_" + string(rune('a'+i)), SessionID: sess.SessionID, Kind: msg.SignalKindQuestion,
			Source: msg.SignalSourceDerived, Surface: msg.SignalSurfaceKanban, Title: "continue?",
			State: msg.SignalStateAnswered, Answer: &msg.SignalAnswer{Text: autoSignoffAnswer}, ResolvedAt: &now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stubClassifier(t, srv, map[string]any{"kind": "question", "title": "Shall I continue?"})
	stubTriage(t, srv, map[string]any{"disposition": "signoff", "reason": "permission"}, nil)

	srv.onTurnEnd(sess.SessionID, turnEndEvent("Done with that step. Shall I continue?"), msg.SessionIdle)

	open := openSignals(t, st, sess.SessionID)
	if len(open) != 1 {
		t.Fatalf("open signals = %d, want 1: the fourth sign-off reaches a person, a worker that keeps asking is looping", len(open))
	}
	count, err := srv.autoContinueCount(sess.SessionID)
	if err != nil || count != maxAutoContinuesPerSession {
		t.Errorf("auto-continue count = %d (%v), want %d unchanged", count, err, maxAutoContinuesPerSession)
	}
}

func TestAnsweringASurfacedQuestionEndsTheCardsWait(t *testing.T) {
	card := &cardStub{}
	srv, st := testServerWithKanban(t, card.handler("card-5"))
	sess := seedWorkerSession(t, st, "demo-builder-6", msg.PurposeDispatcher)
	sig := &msg.Signal{
		ID: "sig_q", SessionID: sess.SessionID, Kind: msg.SignalKindQuestion, Source: msg.SignalSourceDerived,
		Surface: msg.SignalSurfaceKanban, Title: "Include discontinued?", State: msg.SignalStateOpen, LinkedTodoID: "card-5",
	}
	if err := st.CreateSignal(sig); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"state":"dismissed"}`)
	req := httptest.NewRequest("POST", "/signals/sig_q/resolve", body)
	req.SetPathValue("id", "sig_q")
	rec := httptest.NewRecorder()
	srv.handleResolveSignal(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	if kinds := card.eventKinds(); len(kinds) != 1 || kinds[0] != "waiting_ended" {
		t.Errorf("card events = %v, want [waiting_ended]", kinds)
	}
}
