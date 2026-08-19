package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// POST /signals/{id}/answer is the one door. These pin what a caller is
// allowed to assume through it, and — more importantly — what it must NOT
// have to know: which producer raised the question, whether a park is still
// live, or which of two transports carries the answer.
//
// The client used to make that choice. It read request_id, then either
// re-fetched the parked tool input and posted it to the hook route, or posted
// text to /send. A request_id says a park EXISTED, not that it is still live,
// so the client was deciding on evidence it did not have.

func answerRequest(t *testing.T, srv *Server, signalID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/signals/"+signalID+"/answer", bytes.NewReader(encoded))
	req.SetPathValue("id", signalID)
	rec := httptest.NewRecorder()
	srv.handleAnswerSignal(rec, req)
	return rec
}

func openQuestionsFor(t *testing.T, srv *Server, bridgeID, requestID string) []string {
	t.Helper()
	signals, err := srv.store.ListSignalsByRequestID(bridgeID, requestID)
	if err != nil {
		t.Fatalf("list signals: %v", err)
	}
	var ids []string
	for _, sig := range signals {
		ids = append(ids, sig.ID)
	}
	return ids
}

func TestAnswerRefusesAPartialAnswerToAMultiQuestionRequest(t *testing.T) {
	// One AskUserQuestion call carries several questions and resolves ONCE.
	// Answering one of them would resolve the whole request with the rest
	// blank, and the agent would read silence as a reply. The card enforced
	// this; the card is not the only caller, so the request does now.
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)
	srv.recordAskUserQuestionSignals("br_1", sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))
	ids := openQuestionsFor(t, srv, "br_1", "hreq_1")
	if len(ids) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(ids))
	}

	rec := answerRequest(t, srv, ids[0], signalAnswerRequest{Answers: map[string]string{ids[0]: "Yes"}})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("incomplete_answer")) {
		t.Errorf("body should name the refusal: %s", rec.Body.String())
	}
	// And it must refuse WITHOUT resolving anything.
	for _, id := range ids {
		sig, err := st.GetSignal(id)
		if err != nil {
			t.Fatalf("get signal: %v", err)
		}
		if sig.State != msg.SignalStateOpen {
			t.Errorf("signal %s = %q after a refused answer, want open", id, sig.State)
		}
	}
}

func TestAnswerRefusesANotificationAndAResolvedQuestion(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_1", msg.SessionTypeInteractive)

	notification := &store.Signal{
		ID: "sig_note", SessionID: "br_1", Kind: msg.SignalKindNotification,
		Source: msg.SignalSourceDerived, Surface: msg.SignalSurfaceChat,
		Title: "Deploy finished", State: msg.SignalStateOpen,
	}
	if err := st.CreateSignal(notification); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	rec := answerRequest(t, srv, notification.ID, signalAnswerRequest{Answers: map[string]string{notification.ID: "ok"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("answering a notification: status = %d, want 400", rec.Code)
	}

	question := &store.Signal{
		ID: "sig_q", SessionID: "br_1", Kind: msg.SignalKindQuestion,
		Source: msg.SignalSourceDerived, Surface: msg.SignalSurfaceChat,
		Title: "Which transport?", State: msg.SignalStateOpen,
	}
	if err := st.CreateSignal(question); err != nil {
		t.Fatalf("create question: %v", err)
	}
	if err := st.ResolveSignal(question.ID, msg.SignalStateDismissed, nil); err != nil {
		t.Fatalf("pre-resolve: %v", err)
	}
	rec = answerRequest(t, srv, question.ID, signalAnswerRequest{Answers: map[string]string{question.ID: "codex"}})
	if rec.Code != http.StatusConflict {
		t.Errorf("answering a resolved question: status = %d, want 409", rec.Code)
	}
}

func TestAnswerIsUnknownSignal404(t *testing.T) {
	srv, _ := testServer(t)
	rec := answerRequest(t, srv, "sig_nope", signalAnswerRequest{Answers: map[string]string{}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAnswerTextRendersOneQuestionVerbatimAndManyLabelled(t *testing.T) {
	// A single answer goes as the user typed it — labelling it would put words
	// in their mouth. Several answers have to say which question each belongs
	// to, because the assistant asked them separately.
	one := []store.Signal{{ID: "a", Title: "Which transport?"}}
	if got := answerTextForMessage(one, map[string]string{"a": "codex"}); got != "codex" {
		t.Errorf("single answer = %q, want %q", got, "codex")
	}

	many := []store.Signal{{ID: "a", Title: "Ship it?"}, {ID: "b", Title: "Which branch?"}}
	got := answerTextForMessage(many, map[string]string{"a": "Yes", "b": "main"})
	want := "Ship it?\nYes\n\nWhich branch?\nmain"
	if got != want {
		t.Errorf("multi answer =\n%q\nwant\n%q", got, want)
	}
}
