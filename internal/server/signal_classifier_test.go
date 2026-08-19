package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// stubClassifier points srv's classifier at a local Anthropic stand-in that
// answers every call with verdict. The captured request body is returned so a
// test can assert what the model was actually asked.
func stubClassifier(t *testing.T, srv *Server, verdict map[string]any) *string {
	t.Helper()
	var captured string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		captured = string(body)
		resp := map[string]any{
			"content": []any{map[string]any{
				"type":  "tool_use",
				"name":  classifierToolName,
				"input": verdict,
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)

	srv.signalClassifier = newSignalClassifier("claude-haiku-4-5", 5*time.Second, 6000, nil, nil)
	srv.signalClassifier.baseURL = ts.URL
	return &captured
}

func openSignals(t *testing.T, st *store.Store, bridgeID string) []store.Signal {
	t.Helper()
	got, err := st.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		t.Fatalf("list open signals: %v", err)
	}
	return got
}

func turnEndEvent(text string) *msg.Event {
	return &msg.Event{Type: msg.EventResult, Result: &msg.ResultEvent{Text: text}}
}

func TestClassifierEnabledFor(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		optOut  map[msg.Harness]bool
		harness msg.Harness
		want    bool
	}{
		{"on by default", "claude-haiku-4-5", nil, msg.HarnessClaudeCode, true},
		{"empty model disables everywhere", "", nil, msg.HarnessClaudeCode, false},
		{"per-harness opt-out", "claude-haiku-4-5", map[msg.Harness]bool{msg.HarnessClaudeCode: true}, msg.HarnessClaudeCode, false},
		{"opt-out is per harness, not global", "claude-haiku-4-5", map[msg.Harness]bool{msg.HarnessClaudeCode: true}, msg.HarnessCodex, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSignalClassifier(tc.model, time.Second, 100, tc.optOut, nil)
			if got := c.enabledFor(tc.harness); got != tc.want {
				t.Errorf("enabledFor(%q) = %v, want %v", tc.harness, got, tc.want)
			}
		})
	}
}

func TestSkipClassifyReason(t *testing.T) {
	plain := &store.Session{SessionID: "br_1"}
	cases := []struct {
		name     string
		sess     *store.Session
		text     string
		state    msg.SessionState
		wantSkip bool
	}{
		{"ordinary settled turn", plain, "Which database should I migrate first?", msg.SessionIdle, false},
		{"heuristic already parked it", plain, "Which database should I migrate first?", msg.SessionAwaitingUser, false},
		{"renamer helper", &store.Session{Purpose: renamerSourceTag}, "Renamed the session, done.", msg.SessionIdle, true},
		{"empty final message", plain, "   ", msg.SessionIdle, true},
		{"too short to carry a signal", plain, "ok", msg.SessionIdle, true},
		{"turn errored", plain, "Which database should I migrate first?", msg.SessionError, true},
		{"another turn already opened", plain, "Which database should I migrate first?", msg.SessionToolRunning, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skipClassifyReason(tc.sess, tc.text, tc.state)
			if (got != "") != tc.wantSkip {
				t.Errorf("skipClassifyReason = %q, wantSkip=%v", got, tc.wantSkip)
			}
		})
	}
}

func TestOnTurnEndRecordsDerivedQuestion(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_q", msg.SessionTypeInteractive)
	stubClassifier(t, srv, map[string]any{
		"kind":  "question",
		"title": "Should I drop the legacy column?",
		"body":  "It has no readers left but two writers.",
		"options": []any{
			map[string]any{"label": "Drop it", "description": "irreversible"},
			map[string]any{"label": "Leave it"},
		},
	})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("…so: should I drop the legacy column?"), msg.SessionIdle)

	got := openSignals(t, st, sess.SessionID)
	if len(got) != 1 {
		t.Fatalf("open signals = %d, want 1", len(got))
	}
	sig := got[0]
	if sig.Kind != msg.SignalKindQuestion {
		t.Errorf("kind = %q, want question", sig.Kind)
	}
	if sig.Source != msg.SignalSourceDerived {
		t.Errorf("source = %q, want derived", sig.Source)
	}
	// The record that tells the two resolve paths apart: a derived signal has
	// no parked hook behind it, so it must carry no request id. A client that
	// saw one here would post to the hook-resolve verb and 404.
	if sig.RequestID != "" {
		t.Errorf("request_id = %q, want empty on a derived signal", sig.RequestID)
	}
	if sig.Title != "Should I drop the legacy column?" {
		t.Errorf("title = %q", sig.Title)
	}
	if len(sig.Options) != 2 || sig.Options[0].Label != "Drop it" || sig.Options[0].Value != "Drop it" {
		t.Errorf("options = %+v", sig.Options)
	}
	if !sig.AllowFreeform {
		t.Error("allow_freeform = false; a derived question resolves through /send, which takes free text")
	}
	if sig.Surface != msg.SignalSurfaceChat {
		t.Errorf("surface = %q, want chat", sig.Surface)
	}
}

func TestOnTurnEndRecordsNotificationWithSeverity(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_n", msg.SessionTypeInteractive)
	stubClassifier(t, srv, map[string]any{
		"kind":     "notification",
		"title":    "The nightly migration failed",
		"body":     "Rolled back cleanly; the schema is unchanged.",
		"severity": "warn",
	})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("The nightly migration failed and I rolled it back."), msg.SessionIdle)

	got := openSignals(t, st, sess.SessionID)
	if len(got) != 1 {
		t.Fatalf("open signals = %d, want 1", len(got))
	}
	sig := got[0]
	if sig.Kind != msg.SignalKindNotification {
		t.Errorf("kind = %q, want notification", sig.Kind)
	}
	if sig.Severity != msg.SignalSeverityWarn {
		t.Errorf("severity = %q, want warn", sig.Severity)
	}
	if len(sig.Options) != 0 {
		t.Errorf("options = %+v; a notification asks nothing", sig.Options)
	}
	if sig.AllowFreeform {
		t.Error("allow_freeform = true on a notification")
	}
}

func TestOnTurnEndNeitherRecordsNothing(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_none", msg.SessionTypeInteractive)
	stubClassifier(t, srv, map[string]any{"kind": "neither"})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("Done — the tests pass and I pushed the branch."), msg.SessionIdle)

	if got := openSignals(t, st, sess.SessionID); len(got) != 0 {
		t.Fatalf("open signals = %d, want 0 — an ordinary completion is not a signal", len(got))
	}
}

func TestOnTurnEndSupersedesTheEarlierDerivedSignal(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_super", msg.SessionTypeInteractive)
	stubClassifier(t, srv, map[string]any{"kind": "question", "title": "Which one?"})

	srv.onTurnEnd(sess.SessionID, turnEndEvent("There are two candidates — which one?"), msg.SessionIdle)
	first := openSignals(t, st, sess.SessionID)
	if len(first) != 1 {
		t.Fatalf("after first turn: open signals = %d, want 1", len(first))
	}

	srv.onTurnEnd(sess.SessionID, turnEndEvent("Actually there are three now — which one?"), msg.SessionIdle)

	// Exactly one derived row stays open: the newest. Nothing else resolves a
	// derived signal until P4 adds the signal-level verb, so without
	// supersession every turn would leave another stale row in the inbox.
	after := openSignals(t, st, sess.SessionID)
	if len(after) != 1 {
		t.Fatalf("after second turn: open signals = %d, want 1", len(after))
	}
	if after[0].ID == first[0].ID {
		t.Error("the newest turn did not mint a fresh signal")
	}

	superseded, err := st.GetSignal(first[0].ID)
	if err != nil {
		t.Fatalf("get superseded signal: %v", err)
	}
	if superseded.State != msg.SignalStateDismissed {
		t.Errorf("superseded signal state = %q, want dismissed", superseded.State)
	}
}

func TestOnTurnEndYieldsToAnOpenToolQuestion(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_tool", msg.SessionTypeInteractive)
	// Park it for real. The invariant under test is that a question with a
	// LIVE parked hook owns the ask — a request_id alone says a park existed,
	// not that anything is still waiting on it, and the fixture has to carry
	// the difference or it is testing a case production never produces.
	srv.parkedAsks.park(sess.SessionID, "hreq_1")
	srv.recordAskUserQuestionSignals(sess.SessionID, sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))
	before := len(openSignals(t, st, sess.SessionID))

	stubClassifier(t, srv, map[string]any{"kind": "question", "title": "Also, which branch?"})
	srv.onTurnEnd(sess.SessionID, turnEndEvent("Also, which branch should I use?"), msg.SessionAwaitingUser)

	// A parked tool ask is a structured question with a real resolve verb.
	// A derived row alongside it is a second, weaker copy of the same demand.
	if got := len(openSignals(t, st, sess.SessionID)); got != before {
		t.Errorf("open signals = %d, want %d — the tool park already owns this ask", got, before)
	}
}

func TestOnTurnEndSkipsDisabledClassifierWithoutCallingIt(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_off", msg.SessionTypeInteractive)
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(ts.Close)
	srv.signalClassifier = newSignalClassifier("", time.Second, 6000, nil, nil)
	srv.signalClassifier.baseURL = ts.URL

	srv.onTurnEnd(sess.SessionID, turnEndEvent("Should I proceed with the migration?"), msg.SessionIdle)

	if called {
		t.Error("classifier called with an empty model; the off switch does not switch it off")
	}
	if got := openSignals(t, st, sess.SessionID); len(got) != 0 {
		t.Errorf("open signals = %d, want 0", len(got))
	}
}

func TestOnTurnEndSendsTheTailOfALongTurn(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_long", msg.SessionTypeInteractive)
	captured := stubClassifier(t, srv, map[string]any{"kind": "neither"})
	srv.signalClassifier.maxChars = 80

	question := "So which of the two should I use?"
	srv.onTurnEnd(sess.SessionID, turnEndEvent(strings.Repeat("filler line\n", 200)+question), msg.SessionIdle)

	// The question a human has to answer is at the end of a turn, so a
	// truncation that kept the head would classify the file dump instead.
	if !strings.Contains(*captured, question) {
		t.Errorf("classify request dropped the tail of the turn:\n%s", *captured)
	}
}

func TestAnswerDerivedQuestionsClosesTheQuestionButNotTheNotification(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_send", msg.SessionTypeInteractive)

	stubClassifier(t, srv, map[string]any{"kind": "question", "title": "Which branch?"})
	srv.onTurnEnd(sess.SessionID, turnEndEvent("Two branches look right — which branch?"), msg.SessionIdle)
	question := openSignals(t, st, sess.SessionID)[0]

	note := &msg.Signal{
		ID: "sig_note", SessionID: sess.SessionID, Kind: msg.SignalKindNotification,
		Source: msg.SignalSourceDerived, Surface: msg.SignalSurfaceChat, Title: "Migration finished",
	}
	if err := st.CreateSignal(note); err != nil {
		t.Fatalf("create notification: %v", err)
	}

	srv.closeQuestionsAnsweredByMessage(sess.SessionID, "  use main  ")

	answered, err := st.GetSignal(question.ID)
	if err != nil {
		t.Fatalf("get question: %v", err)
	}
	if answered.State != msg.SignalStateAnswered {
		t.Errorf("question state = %q, want answered — /send is the derived question's resolve verb", answered.State)
	}
	if answered.Answer == nil || answered.Answer.Text != "use main" {
		t.Errorf("answer = %+v, want the sent message", answered.Answer)
	}

	// A notification is acknowledged, not answered. Sending a message says
	// nothing about whether the human read it.
	stillOpen, err := st.GetSignal(note.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if stillOpen.State != msg.SignalStateOpen {
		t.Errorf("notification state = %q, want open", stillOpen.State)
	}
}

func TestAnswerDerivedQuestionsLeavesToolParksToTheHookVerb(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_park", msg.SessionTypeInteractive)
	srv.parkedAsks.park(sess.SessionID, "hreq_1")
	srv.recordAskUserQuestionSignals(sess.SessionID, sess, "hreq_1", json.RawMessage(askUserQuestionToolInput))

	srv.closeQuestionsAnsweredByMessage(sess.SessionID, "whatever")

	// A tool-sourced signal is resolved by resolving its parked hook. Closing
	// it on a bare /send would leave the hook parked with its record already
	// marked answered.
	for _, sig := range openSignals(t, st, sess.SessionID) {
		if sig.Source == msg.SignalSourceTool && sig.State != msg.SignalStateOpen {
			t.Errorf("tool signal %s closed by /send", sig.ID)
		}
	}
	if got := len(openSignals(t, st, sess.SessionID)); got != 2 {
		t.Errorf("open tool signals = %d, want 2", got)
	}
}

func TestClassifierTrimKeepsTail(t *testing.T) {
	c := newSignalClassifier("m", time.Second, 10, nil, nil)
	if got := c.trim("short"); got != "short" {
		t.Errorf("trim under the cap changed the text: %q", got)
	}
	got := c.trim("0123456789abcdefghij")
	if !strings.HasSuffix(got, "abcdefghij") {
		t.Errorf("trim = %q, want it to end with the last 10 runes", got)
	}
}

// The other half of the pair above, and the case that had no coverage: a tool
// question whose park is GONE.
//
// A session is a thread. Once the park has died — its process reaped, its
// connection dropped — that question can no longer be delivered to the tool
// call that asked it, and an answer sent now would land at the end of a
// conversation that has moved on. It is exactly as stale as a derived one, and
// the next turn has to retire it.
//
// This became reachable when a question started outliving its session. Before
// that, a dead park took its rows down with it, so "tool-sourced" and "still
// live" were the same thing and the code could test either. They are now
// different, and only "still live" is the one that matters.
func TestAStaleToolQuestionIsRetiredLikeAnyOther(t *testing.T) {
	t.Run("a message closes it", func(t *testing.T) {
		srv, st := testServer(t)
		sess := newSessionForSignals(t, st, "br_dead", msg.SessionTypeInteractive)
		// No park: the process that asked is gone.
		srv.recordAskUserQuestionSignals(sess.SessionID, sess, "hreq_dead", json.RawMessage(askUserQuestionToolInput))

		srv.closeQuestionsAnsweredByMessage(sess.SessionID, "use main")

		if got := len(openSignals(t, st, sess.SessionID)); got != 0 {
			t.Errorf("open questions = %d, want 0 — a dead park cannot be answered later", got)
		}
	})

	t.Run("a turn-end supersedes it", func(t *testing.T) {
		srv, st := testServer(t)
		sess := newSessionForSignals(t, st, "br_dead2", msg.SessionTypeInteractive)
		srv.recordAskUserQuestionSignals(sess.SessionID, sess, "hreq_dead", json.RawMessage(askUserQuestionToolInput))

		srv.supersedeStaleQuestions(sess.SessionID)

		if got := len(openSignals(t, st, sess.SessionID)); got != 0 {
			t.Errorf("open questions = %d, want 0 — the thread moved past them", got)
		}
	})

	t.Run("a live park is never retired", func(t *testing.T) {
		srv, st := testServer(t)
		sess := newSessionForSignals(t, st, "br_live", msg.SessionTypeInteractive)
		srv.parkedAsks.park(sess.SessionID, "hreq_live")
		srv.recordAskUserQuestionSignals(sess.SessionID, sess, "hreq_live", json.RawMessage(askUserQuestionToolInput))

		srv.supersedeStaleQuestions(sess.SessionID)
		srv.closeQuestionsAnsweredByMessage(sess.SessionID, "unrelated")

		// Something is still blocked on this one. Closing it would destroy a
		// real pending tool call.
		if got := len(openSignals(t, st, sess.SessionID)); got != 2 {
			t.Errorf("open questions = %d, want 2 — a live park owns its ask", got)
		}
	})
}
