package rulestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBridge serves GET /sessions from a list and GET /session-events from a
// channel the test writes frames into.
type fakeBridge struct {
	mu       sync.Mutex
	sessions []json.RawMessage
	frames   chan string
	token    string
}

func (b *fakeBridge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-LLM-Bridge-Service-Token") != b.token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		json.NewEncoder(w).Encode(b.sessions)
	})
	mux.HandleFunc("GET /session-events", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-LLM-Bridge-Service-Token") != b.token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: hello\ndata: {\"stream_id\":\"test\",\"resume\":\"none\"}\n\n: keepalive\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case frame := <-b.frames:
				fmt.Fprint(w, frame)
				w.(http.Flusher).Flush()
			}
		}
	})
	return mux
}

var frameSeq int

func upsertFrame(raw json.RawMessage) string {
	frameSeq++
	return fmt.Sprintf("id: test-%d\nevent: upsert\ndata: {\"type\":\"upsert\",\"session\":%s}\n\n", frameSeq, raw)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTheReactorDecidesOncePerStateEntryFromTheSeedAndTheFeed(t *testing.T) {
	s := newTestStore(t)
	rule, err := s.CreateRule("close completed cards", "", SourceSession, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(rule.ID, json.RawMessage(curatorDone()), "", testAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(rule.ID, 1, ModeShadow, testAdmin, "test"); err != nil {
		t.Fatal(err)
	}

	bridgeState := &fakeBridge{token: "synthetic-token", frames: make(chan string, 16), sessions: []json.RawMessage{
		session("already-done", "completed", "chat", "2026-10-19T09:00:00Z"),
		session("running", "tool_running", "chat", "2026-10-19T09:00:00Z"),
		session("gone-later", "idle", "chat", "2026-10-19T09:00:00Z"),
	}}
	server := httptest.NewServer(bridgeState.handler(t))
	defer server.Close()
	facts := &fakeFacts{placements: map[string][]CardPlacement{
		"already-done": {{CardID: "card-1", BoardID: testBoard, ColumnID: testInProgress}},
		"running":      {{CardID: "card-2", BoardID: testBoard, ColumnID: testInProgress}},
	}}
	reactor := NewReactor(s, NewLLMBridge(server.URL, "synthetic-token"), facts, &fakeCaller{}, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reactor.Run(ctx)

	runsFor := func(subjectID string) []Run {
		runs, err := s.ListRuns(RunFilter{SubjectID: subjectID})
		if err != nil {
			t.Fatal(err)
		}
		return runs
	}
	waitFor(t, "the seed to decide about the session that was already completed", func() bool { return len(runsFor("already-done")) == 1 })
	if runs := runsFor("running"); len(runs) != 0 {
		t.Fatalf("a running session must not match: %+v", runs)
	}

	// The running session completes: the feed decides it within moments.
	bridgeState.frames <- upsertFrame(session("running", "completed", "chat", "2026-10-19T09:05:00Z"))
	waitFor(t, "the feed to decide about the session that completed", func() bool { return len(runsFor("running")) == 1 })
	run := runsFor("running")[0]
	if run.Cause != CauseFeed || run.Outcome != OutcomeShadowed || run.TriggerKey != "completed#2" {
		t.Fatalf("unexpected run %+v", run)
	}
	if len(run.PlannedEffects) != 1 || run.PlannedEffects[0].Path != "/api/cards/card-2/move" {
		t.Fatalf("planned effects: %+v", run.PlannedEffects)
	}

	// The same state again, with a later updated_at: no second decision.
	bridgeState.frames <- upsertFrame(session("running", "completed", "chat", "2026-10-19T09:06:00Z"))
	// A delete frame, then a marker frame so the test knows both were read.
	bridgeState.frames <- "id: test-del\nevent: delete\ndata: {\"type\":\"delete\",\"session_id\":\"gone-later\"}\n\n"
	bridgeState.frames <- upsertFrame(session("marker", "idle", "chat", "2026-10-19T09:07:00Z"))
	waitFor(t, "the marker frame", func() bool { _, err := s.GetSubject(SubjectTypeSession, "marker"); return err == nil })
	if runs := runsFor("running"); len(runs) != 1 {
		t.Fatalf("a repeated frame in the same state must not decide again: %d runs", len(runs))
	}
	if _, err := s.GetSubject(SubjectTypeSession, "gone-later"); err == nil {
		t.Fatal("a deleted session must be forgotten")
	}

	// A signal frame and a frame type nobody knows yet: neither drops the
	// feed, and the unknown one is counted.
	bridgeState.frames <- "event: signal\ndata: {\"type\":\"signal\",\"session_id\":\"running\"}\n\n"
	bridgeState.frames <- "event: brand_new\ndata: {}\n\n"

	// Resumed and completed again: a new episode, so a new decision.
	bridgeState.frames <- upsertFrame(session("running", "model_generating", "chat", "2026-10-19T09:10:00Z"))
	bridgeState.frames <- upsertFrame(session("running", "completed", "chat", "2026-10-19T09:20:00Z"))
	waitFor(t, "a second decision after the session completed again", func() bool { return len(runsFor("running")) == 2 })

	status := reactor.Status()
	if !status.FeedConnected || status.LastSeedCount != 3 || status.ErrorsSinceBoot != 0 || status.UnknownFrames["brand_new"] != 1 || status.UnknownFrames["signal"] != 0 {
		t.Fatalf("status %+v", status)
	}
}

func TestTheSweepNoticesThatTimeHasPassed(t *testing.T) {
	s := newTestStore(t)
	rule, err := s.CreateRule("archive idle sessions", "", SourceSession, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(rule.ID, json.RawMessage(`{"conditions":[
		{"kind":"field_in","field":"state","values":["idle"]},
		{"kind":"field_not_in","field":"folder_name","values":["Archive"]},
		{"kind":"updated_before","age":"48h"}],
		"effects":[{"kind":"move_session_to_folder","folder":"Archive"}]}`), "", testAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(rule.ID, 1, ModeShadow, testAdmin, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveSubjects([]SubjectObservation{{Type: SubjectTypeSession, ID: "quiet", State: "idle", Snapshot: session("quiet", "idle", "chat", "2026-10-19T09:00:00Z")}}); err != nil {
		t.Fatal(err)
	}
	reactor := NewReactor(s, nil, &fakeFacts{}, &fakeCaller{}, time.Hour)
	reactor.clock = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	if err := reactor.Sweep(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.ListRuns(RunFilter{}); len(runs) != 0 {
		t.Fatalf("one day idle is not two: %+v", runs)
	}
	reactor.clock = func() time.Time { return time.Date(2026, 10, 21, 9, 0, 1, 0, time.UTC) }
	if err := reactor.Sweep(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := reactor.Sweep(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.ListRuns(RunFilter{})
	if len(runs) != 1 || runs[0].Cause != CauseSweep || runs[0].PlannedEffects[0].Path != "/sessions/quiet/folder" {
		t.Fatalf("want one sweep run that archives the session, got %+v", runs)
	}
}

func TestActivatingARuleChecksSessionsAlreadyInItsState(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ObserveSubjects([]SubjectObservation{{Type: SubjectTypeSession, ID: "finished-earlier", State: "completed", Snapshot: session("finished-earlier", "completed", "chat", "2026-10-19T09:00:00Z")}}); err != nil {
		t.Fatal(err)
	}
	facts := &fakeFacts{placements: map[string][]CardPlacement{"finished-earlier": {{CardID: "card-9", BoardID: testBoard, ColumnID: testInProgress}}}}
	reactor := NewReactor(s, nil, facts, &fakeCaller{}, time.Hour)
	mux := http.NewServeMux()
	RegisterHandlers(context.Background(), mux, s, reactor, nil)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	if rec := call("POST", "/rules", `{"name":"close","source":"llm-bridge-server.session","created_by":"principal_000001"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/rules/rule_000001/revisions", `{"definition":`+curatorDone()+`,"created_by":"principal_000001"}`); rec.Code != http.StatusCreated {
		t.Fatalf("revision: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/rules/rule_000001/activate", `{"revision":1,"mode":"loud","acted_by":"principal_000001","reason":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown mode must be a 400: %d", rec.Code)
	}
	if rec := call("POST", "/rules/rule_000001/activate", `{"revision":1,"mode":"shadow","acted_by":"principal_000001","reason":"test"}`); rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, "the activation check to decide about the session completed before the rule existed", func() bool {
		runs, _ := s.ListRuns(RunFilter{SubjectID: "finished-earlier"})
		return len(runs) == 1
	})
	if rec := call("GET", "/runs?rule_id=rule_000001", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "card-9") {
		t.Fatalf("runs: %d %s", rec.Code, rec.Body)
	}
}

func TestActivityAndSummaryShowWhatTheReactorDid(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ObserveSubjects([]SubjectObservation{{Type: SubjectTypeSession, ID: "done", State: "completed", Snapshot: session("done", "completed", "chat", "2026-10-19T09:00:00Z")}}); err != nil {
		t.Fatal(err)
	}
	facts := &fakeFacts{placements: map[string][]CardPlacement{"done": {{CardID: "card-1", BoardID: testBoard, ColumnID: testInProgress}}}}
	reactor := NewReactor(s, nil, facts, &fakeCaller{}, time.Hour)
	mux := http.NewServeMux()
	RegisterHandlers(context.Background(), mux, s, reactor, nil)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	call("POST", "/rules", `{"name":"close","source":"llm-bridge-server.session","created_by":"principal_000001"}`)
	call("POST", "/rules/rule_000001/revisions", `{"definition":`+curatorDone()+`,"created_by":"principal_000001"}`)
	call("POST", "/rules/rule_000001/activate", `{"revision":1,"mode":"shadow","acted_by":"principal_000001","reason":"test"}`)
	waitFor(t, "the activation check", func() bool { runs, _ := s.ListRuns(RunFilter{}); return len(runs) == 1 })
	if err := func() error {
		reactor.sweeping.Lock()
		defer reactor.sweeping.Unlock()
		return reactor.Sweep(context.Background(), false)
	}(); err != nil {
		t.Fatal(err)
	}

	var activity []Activity
	rec := call("GET", "/activity", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &activity); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("activity: %d %s", rec.Code, rec.Body)
	}
	if len(activity) < 2 || activity[0].Kind != ActivitySweep || activity[len(activity)-1].Kind != ActivityRun {
		t.Fatalf("want the run first and the sweep last-written (newest first): %+v", activity)
	}
	var newer []Activity
	json.Unmarshal(call("GET", fmt.Sprintf("/activity?after=%d", activity[0].Seq), "").Body.Bytes(), &newer)
	if len(newer) != 0 {
		t.Fatalf("after the newest seq there is nothing: %+v", newer)
	}
	if rec := call("GET", "/activity?kind=gossip", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown kind must be a 400: %d", rec.Code)
	}

	var summary struct {
		Rules []RuleSummary `json:"rules"`
	}
	rec = call("GET", "/summary", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil || len(summary.Rules) != 1 || summary.Rules[0].RunsTotal != 1 || summary.Rules[0].RunsLast24h != 1 || len(summary.Rules[0].Revisions) != 1 {
		t.Fatalf("summary: %d %s", rec.Code, rec.Body)
	}
}

// fakeCaller is the owners, answering every call with status, and recording
// what was called.
type fakeCaller struct {
	mu     sync.Mutex
	status int
	calls  []PlannedEffect
}

func (f *fakeCaller) Call(_ context.Context, effect PlannedEffect) EffectResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, effect)
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return EffectResult{Status: status, Body: `{}`, CalledAt: 1}
}

func liveRule(t *testing.T, s *Store, definition string) {
	t.Helper()
	rule, err := s.CreateRule("live rule", "", SourceSession, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(rule.ID, json.RawMessage(definition), "", testAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(rule.ID, 1, ModeLive, testAdmin, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestALiveRuleMakesItsCallsOnceAndRecordsTheAnswers(t *testing.T) {
	s := newTestStore(t)
	liveRule(t, s, `{"conditions":[{"kind":"field_in","field":"state","values":["completed"]}],
		"effects":[{"kind":"add_session_tag","tag":"done"},{"kind":"send_session_message","text":"thanks"}]}`)
	subjects, err := s.ObserveSubjects([]SubjectObservation{{Type: SubjectTypeSession, ID: "s1", State: "completed", Snapshot: session("s1", "completed", "chat", "2026-10-20T09:00:00Z")}})
	if err != nil {
		t.Fatal(err)
	}
	caller := &fakeCaller{}
	reactor := NewReactor(s, nil, &fakeFacts{}, caller, time.Hour)
	rules, _ := s.ActiveRules(SourceSession)
	reactor.decide(context.Background(), rules[0], subjects[0], CauseFeed)
	reactor.decide(context.Background(), rules[0], subjects[0], CauseSweep)

	if len(caller.calls) != 2 {
		t.Fatalf("a live run calls each effect once, and a second decision calls nothing: %d calls", len(caller.calls))
	}
	if caller.calls[1].Body.(map[string]any)["client_request_id"] == nil {
		t.Fatal("a live send must carry its client_request_id")
	}
	runs, _ := s.ListRuns(RunFilter{})
	if len(runs) != 1 || runs[0].Outcome != OutcomeApplied || runs[0].Mode != ModeLive {
		t.Fatalf("runs %+v", runs)
	}
	for _, effect := range runs[0].PlannedEffects {
		if effect.Result == nil || effect.Result.Status != http.StatusOK {
			t.Fatalf("every answer is recorded on the run: %+v", effect)
		}
	}
}

func TestALiveRunStopsAtTheFirstRefusalAndSaysSo(t *testing.T) {
	s := newTestStore(t)
	liveRule(t, s, `{"conditions":[{"kind":"field_in","field":"state","values":["completed"]}],
		"effects":[{"kind":"add_session_tag","tag":"a"},{"kind":"add_session_tag","tag":"b"}]}`)
	subjects, _ := s.ObserveSubjects([]SubjectObservation{{Type: SubjectTypeSession, ID: "s1", State: "completed", Snapshot: session("s1", "completed", "chat", "2026-10-20T09:00:00Z")}})
	caller := &fakeCaller{status: http.StatusConflict}
	reactor := NewReactor(s, nil, &fakeFacts{}, caller, time.Hour)
	rules, _ := s.ActiveRules(SourceSession)
	reactor.decide(context.Background(), rules[0], subjects[0], CauseFeed)

	if len(caller.calls) != 1 {
		t.Fatalf("calls after a refusal must not be made: %d", len(caller.calls))
	}
	runs, _ := s.ListRuns(RunFilter{})
	if runs[0].Outcome != OutcomeFailed || runs[0].PlannedEffects[0].Result.Status != http.StatusConflict || runs[0].PlannedEffects[1].Result != nil {
		t.Fatalf("run %+v", runs[0])
	}
	if status := reactor.Status(); status.ErrorsSinceBoot != 1 || !strings.Contains(status.LastError, "409") {
		t.Fatalf("a refused call is an error on /health: %+v", status)
	}
}

func TestARunLeftApplyingIsMarkedInterruptedNotRepeated(t *testing.T) {
	s := newTestStore(t)
	liveRule(t, s, `{"conditions":[{"kind":"field_in","field":"state","values":["completed"]}],"effects":[{"kind":"add_session_tag","tag":"a"}]}`)
	rules, _ := s.ActiveRules(SourceSession)
	run := &Run{RuleID: rules[0].Rule.ID, RuleRevisionID: rules[0].Revision.ID, SubjectType: SubjectTypeSession, SubjectID: "s1",
		TriggerKey: "completed#1", Mode: ModeLive, Cause: CauseFeed, Facts: json.RawMessage(`{}`), Outcome: OutcomeApplying, DecidedAt: 1}
	if err := s.RecordRun(run); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkInterruptedRuns(); err != nil || n != 1 {
		t.Fatalf("marked %d, %v", n, err)
	}
	if err := s.FinishRun(run.ID, OutcomeApplied, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("an interrupted run cannot be finished as applied: %v", err)
	}
}

func TestOwnerCallsSendTheTokenAndBodyAndKeepTheAnswer(t *testing.T) {
	var gotToken, gotBody, gotType string
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotType = r.Header.Get("X-Kanban-Store-Service-Token"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"tag":"x"}`))
	}))
	defer owner.Close()
	calls := NewOwnerCalls(NewLLMBridge("http://unused", "b"), NewKanbanStore(owner.URL, "kanban-token"))
	result := calls.Call(context.Background(), PlannedEffect{Service: "kanban-store", Method: "POST", Path: "/api/entities/session/s1/tags", Body: map[string]any{"tag": "x"}})
	if !result.accepted() || result.Status != http.StatusCreated || result.Body != `{"tag":"x"}` {
		t.Fatalf("result %+v", result)
	}
	if gotToken != "kanban-token" || gotBody != `{"tag":"x"}` || gotType != "application/json" {
		t.Fatalf("token %q body %q type %q", gotToken, gotBody, gotType)
	}
	if unknown := calls.Call(context.Background(), PlannedEffect{Service: "mailstack", Method: "POST", Path: "/x"}); unknown.accepted() || unknown.Error == "" {
		t.Fatalf("an unknown service must not be called: %+v", unknown)
	}
}
