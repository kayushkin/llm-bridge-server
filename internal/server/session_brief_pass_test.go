package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// stubSessionBrief points the brief pass at a runner that answers every call
// with verdict and records each prompt it was sent.
func stubSessionBrief(t *testing.T, srv *Server, verdict map[string]any) *[]msg.OneShotRequest {
	t.Helper()
	var calls []msg.OneShotRequest
	runner := func(_ context.Context, request msg.OneShotRequest) ([]byte, error) {
		calls = append(calls, request)
		parsed, err := json.Marshal(verdict)
		if err != nil {
			return nil, err
		}
		return json.Marshal(msg.OneShotResponse{Parsed: parsed})
	}
	srv.signalClassifier = newSignalClassifier("turn-end-signal", 5*time.Second, 6000, nil, runner)
	return &calls
}

func storeChatTurn(t *testing.T, st *store.Store, sessionID, userMessage string, assistantTexts ...string) {
	t.Helper()
	write := func(event msg.Event) {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.StoreEvent(sessionID, string(event.Type), "", "", data); err != nil {
			t.Fatalf("store event: %v", err)
		}
	}
	write(msg.Event{Type: msg.EventUserMessage, BridgeSessionID: sessionID, Result: &msg.ResultEvent{Text: userMessage}})
	for _, text := range assistantTexts {
		write(msg.Event{Type: msg.EventBlock, BridgeSessionID: sessionID, Block: &msg.BlockEvent{
			Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: text}},
		}})
	}
}

func TestTheBriefPassWritesTheFirstBriefAndALooseEnd(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_brief", msg.SessionTypeInteractive)
	storeChatTurn(t, st, sess.SessionID, "Move the nightly backup to 02:00",
		"Looking at the scheduler. By the way, the disk is at 91%; want me to look?", "Moved job 81 to 02:00 Pacific.")
	calls := stubSessionBrief(t, srv, map[string]any{
		"overall_goal": "Run the nightly backup at 02:00", "goal_changed": false,
		"current_step": "Moved job 81; nothing pending", "turn_change": "scheduler job 81 now runs at 02:00",
		"settled": []any{},
		"new_loose_ends": []any{
			map[string]any{"kind": "offer", "text": "want me to look at the disk at 91%?"},
			map[string]any{"kind": "made_up_kind", "text": "dropped"},
		},
	})

	srv.updateSessionBriefAtTurnEnd(sess.SessionID, msg.SessionIdle)

	if len(*calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(*calls))
	}
	prompt := (*calls)[0].Prompt
	for _, want := range []string{"Move the nightly backup", "the disk is at 91%", "Moved job 81", "first brief"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
	if (*calls)[0].Caller != oneShotCallerSessionBrief {
		t.Errorf("caller = %q", (*calls)[0].Caller)
	}
	view, err := st.GetSessionBriefView(sess.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Brief == nil || view.Brief.OverallGoal != "Run the nightly backup at 02:00" || view.Brief.RevisionCount != 1 {
		t.Fatalf("brief = %+v", view.Brief)
	}
	if len(view.Revisions) != 1 || view.Revisions[0].TurnChange != "scheduler job 81 now runs at 02:00" {
		t.Errorf("revisions = %+v", view.Revisions)
	}
	if len(view.LooseEnds) != 1 || view.LooseEnds[0].Kind != msg.LooseEndOffer || view.LooseEnds[0].State != msg.LooseEndOpen {
		t.Errorf("loose ends = %+v; want the one offer, the made-up kind dropped", view.LooseEnds)
	}
}

func TestTheNextTurnSeesTheOpenLooseEndAndCanSettleOnlyThatOne(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_brief2", msg.SessionTypeInteractive)
	storeChatTurn(t, st, sess.SessionID, "first", "Want me to also rotate the logs?")
	stubSessionBrief(t, srv, map[string]any{
		"overall_goal": "goal", "goal_changed": false, "current_step": "step", "turn_change": "",
		"settled": []any{}, "new_loose_ends": []any{map[string]any{"kind": "offer", "text": "rotate the logs?"}},
	})
	srv.updateSessionBriefAtTurnEnd(sess.SessionID, msg.SessionIdle)
	first, _ := st.GetSessionBriefView(sess.SessionID)
	looseEndID := first.LooseEnds[0].ID

	storeChatTurn(t, st, sess.SessionID, "yes rotate them", "Rotated.")
	calls := stubSessionBrief(t, srv, map[string]any{
		"overall_goal": "goal", "goal_changed": false, "current_step": "done", "turn_change": "rotated logs",
		"settled": []any{
			map[string]any{"id": looseEndID, "how": "the person said yes and it rotated them"},
			map[string]any{"id": "loose_NOT_SHOWN", "how": "made up"},
		},
		"new_loose_ends": []any{},
	})
	srv.updateSessionBriefAtTurnEnd(sess.SessionID, msg.SessionIdle)

	if !strings.Contains((*calls)[0].Prompt, looseEndID) {
		t.Errorf("the second turn's prompt does not show the open loose end %s", looseEndID)
	}
	second, _ := st.GetSessionBriefView(sess.SessionID)
	if second.Brief.RevisionCount != 2 || len(second.Revisions) != 2 {
		t.Errorf("revision count = %d, revisions = %d; want 2 and 2", second.Brief.RevisionCount, len(second.Revisions))
	}
	if second.LooseEnds[0].State != msg.LooseEndSettled || second.LooseEnds[0].Resolution == "" {
		t.Errorf("loose end = %+v; want settled with how", second.LooseEnds[0])
	}
}

func TestTheBriefPassSkipsWhatIsNotASettledChatTurn(t *testing.T) {
	cases := []struct {
		name  string
		sess  *store.Session
		state msg.SessionState
		skip  bool
	}{
		{"chat, idle", &store.Session{Type: msg.SessionTypeInteractive}, msg.SessionIdle, false},
		{"chat, awaiting the person", &store.Session{Type: msg.SessionTypeInteractive}, msg.SessionAwaitingUser, false},
		{"autonomous worker", &store.Session{Type: msg.SessionTypeAutonomous}, msg.SessionIdle, true},
		{"session with no type", &store.Session{}, msg.SessionIdle, true},
		{"renamer", &store.Session{Type: msg.SessionTypeInteractive, Purpose: renamerSourceTag}, msg.SessionIdle, true},
		{"errored turn", &store.Session{Type: msg.SessionTypeInteractive}, msg.SessionError, true},
	}
	for _, c := range cases {
		if got := skipSessionBriefReason(c.sess, c.state) != ""; got != c.skip {
			t.Errorf("%s: skipped = %v, want %v", c.name, got, c.skip)
		}
	}
}

func TestTheBriefPassMakesNoCallForAnAutonomousSession(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_worker", msg.SessionTypeAutonomous)
	storeChatTurn(t, st, sess.SessionID, "work", "worked")
	calls := stubSessionBrief(t, srv, map[string]any{})
	srv.updateSessionBriefAtTurnEnd(sess.SessionID, msg.SessionIdle)
	if len(*calls) != 0 {
		t.Errorf("model calls = %d, want none for a worker", len(*calls))
	}
}

func TestALooseEndCanBeDismissedOnceAndOnlyThroughItsOwnSession(t *testing.T) {
	srv, st := testServer(t)
	sess := newSessionForSignals(t, st, "br_dismiss", msg.SessionTypeInteractive)
	other := newSessionForSignals(t, st, "br_other", msg.SessionTypeInteractive)
	if err := st.ApplySessionBriefUpdate(store.SessionBriefUpdate{
		SessionID: sess.SessionID,
		Revision:  msg.SessionBriefRevision{ID: "sbrev_1", OverallGoal: "g"},
		Raised:    []msg.LooseEnd{{ID: "loose_1", Kind: msg.LooseEndQuestion, Text: "which port?"}},
	}); err != nil {
		t.Fatal(err)
	}
	post := func(path string) int {
		recorder := httptest.NewRecorder()
		srv.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		return recorder.Code
	}
	if code := post("/sessions/" + other.SessionID + "/loose-ends/loose_1/dismiss"); code != http.StatusNotFound {
		t.Errorf("dismiss through another session = %d, want 404", code)
	}
	if code := post("/sessions/" + sess.SessionID + "/loose-ends/loose_1/dismiss"); code != http.StatusOK {
		t.Errorf("dismiss = %d, want 200", code)
	}
	if code := post("/sessions/" + sess.SessionID + "/loose-ends/loose_1/dismiss"); code != http.StatusConflict {
		t.Errorf("second dismiss = %d, want 409", code)
	}
}

func TestKeepHeadAndTailKeepsBothEnds(t *testing.T) {
	text := strings.Repeat("a", 50) + strings.Repeat("m", 1000) + strings.Repeat("z", 50)
	got := keepHeadAndTail(text, 200)
	if !strings.HasPrefix(got, strings.Repeat("a", 50)) || !strings.HasSuffix(got, strings.Repeat("z", 50)) {
		t.Errorf("lost an end: %q", got)
	}
}
