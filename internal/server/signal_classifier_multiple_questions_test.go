package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A turn can end on several questions at once. On 2026-10-20 a turn asked how
// to handle pull requests (a, b or c) and how to handle next steps (a, b or
// c); the classifier could return only one question with one list of
// options, and the chat showed two buttons, "PRs: choose a, b, or c" and
// "Next steps: choose a, b, or c", neither of which answered anything
// (sig_01M5D1003TG9GXK3DZPEWYPG57). These tests pin the shape that replaced
// it: one row per question, grouped, answered together.

// twoQuestionVerdict is that turn's verdict as the classifier should give it.
func twoQuestionVerdict() map[string]any {
	return map[string]any{
		"kind": "question",
		"questions": []any{
			map[string]any{
				"question": "How should the projects page handle pull requests?",
				"context":  "No store on this host tracks pull requests.",
				"options": []any{
					map[string]any{"label": "Leave pull requests out"},
					map[string]any{"label": "Have dash ask gh live for each repo"},
					map[string]any{"label": "Add pull requests to repo-store", "description": "The assistant's pick: one owner for the data."},
				},
			},
			map[string]any{
				"question": "How should goals and next steps work?",
				"options": []any{
					map[string]any{"label": "Store next steps in project-store"},
					map[string]any{"label": "Generate suggested next steps with a model"},
					map[string]any{"label": "Both: stored steps, with suggestions as drafts", "description": "The assistant's pick."},
				},
			},
		},
	}
}

func TestTurnAskingTwoQuestionsRecordsOneGroupOfTwo(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_two", msg.SessionTypeInteractive)
	stubClassifier(t, srv, twoQuestionVerdict())

	srv.onTurnEnd(sess.SessionID, turnEndEvent("…Which do you want for PRs and for next steps?"), msg.SessionIdle)

	got := openSignals(t, st, sess.SessionID)
	if len(got) != 2 {
		t.Fatalf("open signals = %d, want one per question (2)", len(got))
	}
	byTitle := map[string]store.Signal{}
	for _, sig := range got {
		byTitle[sig.Title] = sig
	}
	pullRequests, ok := byTitle["How should the projects page handle pull requests?"]
	if !ok {
		t.Fatalf("no row for the pull request question; titles: %v", byTitle)
	}
	nextSteps, ok := byTitle["How should goals and next steps work?"]
	if !ok {
		t.Fatalf("no row for the next steps question; titles: %v", byTitle)
	}

	if pullRequests.RequestID == "" || pullRequests.RequestID != nextSteps.RequestID {
		t.Errorf("request ids = %q and %q, want one shared id so the chat draws and answers them together", pullRequests.RequestID, nextSteps.RequestID)
	}
	if !strings.HasPrefix(pullRequests.RequestID, "dqgroup_") {
		t.Errorf("request id = %q, want a derived group id, never one that looks like a parked hook's", pullRequests.RequestID)
	}
	for _, sig := range got {
		if sig.Source != msg.SignalSourceDerived || sig.Kind != msg.SignalKindQuestion {
			t.Errorf("%s: source=%q kind=%q, want a derived question", sig.ID, sig.Source, sig.Kind)
		}
		if len(sig.Options) != 3 {
			t.Errorf("%q: %d options, want the three the assistant offered for it", sig.Title, len(sig.Options))
		}
		if !sig.AllowFreeform {
			t.Errorf("%q: allow_freeform = false", sig.Title)
		}
	}
	if pullRequests.Body != "No store on this host tracks pull requests." {
		t.Errorf("body = %q, want the question's context", pullRequests.Body)
	}
	if pullRequests.Options[2].Description != "The assistant's pick: one owner for the data." {
		t.Errorf("option description = %q", pullRequests.Options[2].Description)
	}
	if nextSteps.Body != "" {
		t.Errorf("body = %q, want empty for a question with no context", nextSteps.Body)
	}
}

func TestADerivedGroupIsAnsweredWholeAndReachesTheSessionAsOneLabelledMessage(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_two", msg.SessionTypeInteractive)
	stubClassifier(t, srv, twoQuestionVerdict())
	srv.onTurnEnd(sess.SessionID, turnEndEvent("…Which do you want for PRs and for next steps?"), msg.SessionIdle)
	group := openSignals(t, st, sess.SessionID)
	if len(group) != 2 {
		t.Fatalf("open signals = %d, want 2", len(group))
	}

	// One answer of two is refused, and refused without closing anything:
	// sending it would leave the other question unanswerable in its context.
	rec := answerRequest(t, srv, group[0].ID, signalAnswerRequest{Answers: map[string]string{group[0].ID: "Add pull requests to repo-store"}})
	if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte("incomplete_answer")) {
		t.Fatalf("partial answer: status = %d body %s, want 400 incomplete_answer", rec.Code, rec.Body.String())
	}
	if still := openSignals(t, st, sess.SessionID); len(still) != 2 {
		t.Errorf("open signals after a refused answer = %d, want 2", len(still))
	}

	// Both answers become one message with each answer under its question.
	answers := map[string]string{group[0].ID: "first answer", group[1].ID: "second answer"}
	text := answerTextForMessage(group, answers)
	for _, sig := range group {
		if !strings.Contains(text, sig.Title+"\n"+answers[sig.ID]) {
			t.Errorf("message does not carry %q under its question:\n%s", answers[sig.ID], text)
		}
	}
}

func TestQuestionsWithNoTextAreDroppedAndAVerdictOfNoneRecordsNothing(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_blank", msg.SessionTypeInteractive)
	stubClassifier(t, srv, map[string]any{
		"kind": "question",
		"questions": []any{
			map[string]any{"question": "   "},
			map[string]any{"question": "Which branch should I ship from?", "options": []any{
				map[string]any{"label": "main"}, map[string]any{"label": "  "},
			}},
		},
	})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("Which branch should I ship from?"), msg.SessionIdle)

	got := openSignals(t, st, sess.SessionID)
	if len(got) != 1 {
		t.Fatalf("open signals = %d, want 1: a blank question cannot be shown", len(got))
	}
	if got[0].RequestID != "" {
		t.Errorf("request id = %q, want none: one usable question is no group", got[0].RequestID)
	}
	if len(got[0].Options) != 1 || got[0].Options[0].Label != "main" {
		t.Errorf("options = %+v, want only the labelled one", got[0].Options)
	}

	stubClassifier(t, srv, map[string]any{"kind": "question", "questions": []any{map[string]any{"question": ""}}})
	srv.onTurnEnd(sess.SessionID, turnEndEvent("Hmm, which one?"), msg.SessionIdle)
	if got := openSignals(t, st, sess.SessionID); len(got) != 0 {
		t.Errorf("open signals = %d, want 0 for a question verdict with no question text", len(got))
	}
}

func TestClassifierIsAskedForAListOfQuestions(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_schema", msg.SessionTypeInteractive)
	captured := stubClassifier(t, srv, map[string]any{"kind": "neither"})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("Which do you want for PRs and for next steps?"), msg.SessionIdle)

	var request msg.OneShotRequest
	if err := json.Unmarshal([]byte(*captured), &request); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Items struct {
				Required []string `json:"required"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(request.Schema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	questions, ok := schema.Properties["questions"]
	if !ok {
		t.Fatal("schema has no questions property")
	}
	if len(questions.Items.Required) != 1 || questions.Items.Required[0] != "question" {
		t.Errorf("questions items require %v, want [question]", questions.Items.Required)
	}
	if _, ok := schema.Properties["options"]; ok {
		t.Error("schema still has top-level options, which cannot hold two questions' choices")
	}
	for _, rule := range []string{"Never merge them", "never a bare letter or number"} {
		if !strings.Contains(request.SystemPrompt, rule) {
			t.Errorf("system prompt does not carry the rule %q", rule)
		}
	}
}

// stubTriageByQuestion answers each triage call by the question it carries:
// the verdict whose key appears in the prompt wins.
func stubTriageByQuestion(t *testing.T, srv *Server, verdictByQuestionText map[string]map[string]any) {
	t.Helper()
	runner := func(_ context.Context, req msg.OneShotRequest) ([]byte, error) {
		for text, verdict := range verdictByQuestionText {
			if strings.Contains(req.Prompt, text) {
				parsed, err := json.Marshal(verdict)
				if err != nil {
					return nil, err
				}
				return json.Marshal(msg.OneShotResponse{Parsed: parsed, StopReason: "tool_use"})
			}
		}
		t.Fatalf("triage asked about a question the stub does not know:\n%s", req.Prompt)
		return nil, nil
	}
	srv.questionTriage = newQuestionTriage("claude-haiku-4-5", 5*time.Second, runner)
}

func TestWorkerTurnWithOneRealQuestionAndOneSignoffSurfacesBothAsOneGroup(t *testing.T) {
	srv, st := testServerWithKanban(t, (&cardStub{}).handler("card-9"))
	sess := seedWorkerSession(t, st, "br_worker_two", msg.PurposeDispatcher)
	stubClassifier(t, srv, twoQuestionVerdict())
	stubTriageByQuestion(t, srv, map[string]map[string]any{
		"pull requests":  {"disposition": "needs_input", "reason": "a real choice", "audience": "assignee"},
		"goals and next": {"disposition": "signoff", "reason": "the worker said what it would do"},
	})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("…Which do you want for PRs and for next steps?"), msg.SessionIdle)

	got := openSignals(t, st, sess.SessionID)
	if len(got) != 2 {
		t.Fatalf("open signals = %d, want both questions: the person sees what the worker asked in full", len(got))
	}
	if got[0].RequestID == "" || got[0].RequestID != got[1].RequestID {
		t.Errorf("request ids = %q and %q, want one shared id", got[0].RequestID, got[1].RequestID)
	}
	for _, sig := range got {
		wantAudience := msg.SignalAudience("")
		if strings.Contains(sig.Title, "pull requests") {
			wantAudience = msg.SignalAudienceAssignee
		}
		if sig.Audience != wantAudience {
			t.Errorf("%q: audience = %q, want %q (only a question that needs input has one)", sig.Title, sig.Audience, wantAudience)
		}
	}
}

// The test server has no log-store, so the send that would carry the
// automatic sign-off fails. That makes this the failure path: the questions
// reach a person instead, and no answered row is left beside them claiming
// the worker was told to carry on.
func TestWorkerSignoffsThatCannotBeSentAreSurfacedWithNoAnsweredRowsBesideThem(t *testing.T) {
	srv, st := testServerWithKanban(t, (&cardStub{}).handler("card-9"))
	sess := seedWorkerSession(t, st, "br_worker_signoffs", msg.PurposeDispatcher)
	stubClassifier(t, srv, twoQuestionVerdict())
	stubTriageByQuestion(t, srv, map[string]map[string]any{
		"pull requests":  {"disposition": "signoff", "reason": "permission"},
		"goals and next": {"disposition": "signoff", "reason": "permission"},
	})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("…Which do you want for PRs and for next steps?"), msg.SessionIdle)

	open := openSignals(t, st, sess.SessionID)
	if len(open) != 2 {
		t.Fatalf("open signals = %d, want both questions surfaced when the sign-off could not be sent", len(open))
	}
	if open[0].RequestID == "" || open[0].RequestID != open[1].RequestID {
		t.Errorf("request ids = %q and %q, want one shared id", open[0].RequestID, open[1].RequestID)
	}
	answered, err := st.ListSignals(store.SignalFilter{SessionID: sess.SessionID, State: msg.SignalStateAnswered})
	if err != nil {
		t.Fatalf("list answered: %v", err)
	}
	if len(answered) != 0 {
		t.Errorf("answered signals = %d, want 0: nothing was sent, so nothing was answered", len(answered))
	}
}
