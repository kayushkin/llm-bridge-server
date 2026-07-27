package store

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func openQuestion(id, sessionID, title string) *Signal {
	return &Signal{
		ID:          id,
		SessionID:   sessionID,
		SessionType: msg.SessionTypeInteractive,
		Kind:        msg.SignalKindQuestion,
		Source:      msg.SignalSourceTool,
		RequestID:   "hreq_abc",
		Surface:     msg.SignalSurfaceChat,
		Title:       title,
		Options: []msg.SignalOption{
			{Label: "Yes", Value: "Yes", Description: "do it"},
			{Label: "No", Value: "No"},
		},
		AllowFreeform: true,
	}
}

func TestCreateAndGetSignal(t *testing.T) {
	s := testStore(t)

	want := openQuestion("sig_1", "br_1", "Ship it?")
	if err := s.CreateSignal(want); err != nil {
		t.Fatalf("create signal: %v", err)
	}
	if want.State != msg.SignalStateOpen {
		t.Errorf("state should default to open, got %q", want.State)
	}
	if want.CreatedAt.IsZero() {
		t.Error("created_at should be stamped on create")
	}

	got, err := s.GetSignal("sig_1")
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if got.Title != "Ship it?" || got.SessionID != "br_1" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Kind != msg.SignalKindQuestion || got.Source != msg.SignalSourceTool {
		t.Errorf("kind/source lost: kind=%q source=%q", got.Kind, got.Source)
	}
	if got.Surface != msg.SignalSurfaceChat || got.SessionType != msg.SessionTypeInteractive {
		t.Errorf("surface/session_type lost: surface=%q type=%q", got.Surface, got.SessionType)
	}
	if !got.AllowFreeform {
		t.Error("allow_freeform lost")
	}
	if len(got.Options) != 2 || got.Options[0].Label != "Yes" || got.Options[0].Description != "do it" {
		t.Errorf("options lost: %+v", got.Options)
	}
	if got.Answer != nil {
		t.Errorf("open signal should carry no answer, got %+v", got.Answer)
	}
	if got.ResolvedAt != nil {
		t.Errorf("open signal should carry no resolved_at, got %v", got.ResolvedAt)
	}
}

func TestCreateSignalRejectsMissingIdentifiers(t *testing.T) {
	s := testStore(t)

	if err := s.CreateSignal(&Signal{SessionID: "br_1", Title: "x"}); err == nil {
		t.Error("a signal with no id should be rejected")
	}
	if err := s.CreateSignal(&Signal{ID: "sig_1", Title: "x"}); err == nil {
		t.Error("a signal with no session_id should be rejected")
	}
}

func TestResolveSignalStampsAnswer(t *testing.T) {
	s := testStore(t)
	if err := s.CreateSignal(openQuestion("sig_1", "br_1", "Ship it?")); err != nil {
		t.Fatalf("create signal: %v", err)
	}

	answer := &msg.SignalAnswer{Option: "Yes", Text: "Yes, after the smoke test"}
	if err := s.ResolveSignal("sig_1", msg.SignalStateAnswered, answer); err != nil {
		t.Fatalf("resolve signal: %v", err)
	}

	got, err := s.GetSignal("sig_1")
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if got.State != msg.SignalStateAnswered {
		t.Errorf("state = %q, want answered", got.State)
	}
	if got.Answer == nil || got.Answer.Text != "Yes, after the smoke test" || got.Answer.Option != "Yes" {
		t.Errorf("answer not persisted: %+v", got.Answer)
	}
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at should be stamped")
	}
	if time.Since(*got.ResolvedAt) > time.Minute {
		t.Errorf("resolved_at looks wrong: %v", *got.ResolvedAt)
	}
}

// A duplicate resolve must not overwrite the resolution that actually
// happened — the first decision is the real one.
func TestResolveSignalIsNotIdempotentOverwrite(t *testing.T) {
	s := testStore(t)
	if err := s.CreateSignal(openQuestion("sig_1", "br_1", "Ship it?")); err != nil {
		t.Fatalf("create signal: %v", err)
	}
	if err := s.ResolveSignal("sig_1", msg.SignalStateAnswered, &msg.SignalAnswer{Text: "Yes"}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := s.ResolveSignal("sig_1", msg.SignalStateDismissed, nil); err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	got, err := s.GetSignal("sig_1")
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if got.State != msg.SignalStateAnswered {
		t.Errorf("second resolve overwrote the first: state = %q, want answered", got.State)
	}
	if got.Answer == nil || got.Answer.Text != "Yes" {
		t.Errorf("second resolve clobbered the answer: %+v", got.Answer)
	}
}

func TestResolveSignalRejectsOpenState(t *testing.T) {
	s := testStore(t)
	if err := s.CreateSignal(openQuestion("sig_1", "br_1", "Ship it?")); err != nil {
		t.Fatalf("create signal: %v", err)
	}
	if err := s.ResolveSignal("sig_1", msg.SignalStateOpen, nil); err == nil {
		t.Error("resolving TO open should be rejected — open is not a resolution")
	}
}

func TestListSignalsFilters(t *testing.T) {
	s := testStore(t)

	first := openQuestion("sig_1", "br_1", "Ship it?")
	second := openQuestion("sig_2", "br_2", "Roll back?")
	second.Surface = msg.SignalSurfaceKanban
	second.SessionType = msg.SessionTypeAutonomous
	third := &Signal{
		ID:        "sig_3",
		SessionID: "br_1",
		Kind:      msg.SignalKindNotification,
		Source:    msg.SignalSourceDerived,
		Surface:   msg.SignalSurfaceChat,
		Title:     "Migration finished",
		Severity:  msg.SignalSeverityInfo,
	}
	for _, sig := range []*Signal{first, second, third} {
		if err := s.CreateSignal(sig); err != nil {
			t.Fatalf("create %s: %v", sig.ID, err)
		}
	}
	if err := s.ResolveSignal("sig_3", msg.SignalStateAcknowledged, nil); err != nil {
		t.Fatalf("resolve sig_3: %v", err)
	}

	all, err := s.ListSignals(SignalFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list all returned %d, want 3", len(all))
	}

	cases := []struct {
		name   string
		filter SignalFilter
		want   []string
	}{
		{"by session", SignalFilter{SessionID: "br_1"}, []string{"sig_1", "sig_3"}},
		{"by state", SignalFilter{State: msg.SignalStateOpen}, []string{"sig_1", "sig_2"}},
		{"by surface", SignalFilter{Surface: msg.SignalSurfaceKanban}, []string{"sig_2"}},
		{"by kind", SignalFilter{Kind: msg.SignalKindNotification}, []string{"sig_3"}},
		{"session and state", SignalFilter{SessionID: "br_1", State: msg.SignalStateOpen}, []string{"sig_1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListSignals(tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			ids := map[string]bool{}
			for _, sig := range got {
				ids[sig.ID] = true
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d signals %v, want %v", len(got), ids, tc.want)
			}
			for _, want := range tc.want {
				if !ids[want] {
					t.Errorf("%s missing from results %v", want, ids)
				}
			}
		})
	}
}

func TestListSignalsLimit(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"sig_1", "sig_2", "sig_3"} {
		if err := s.CreateSignal(openQuestion(id, "br_1", "q "+id)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	got, err := s.ListSignals(SignalFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2 returned %d rows", len(got))
	}
}

func TestListSignalsByRequestID(t *testing.T) {
	s := testStore(t)

	// Two questions from one parked request, plus an unrelated one.
	for _, id := range []string{"sig_1", "sig_2"} {
		if err := s.CreateSignal(openQuestion(id, "br_1", "q "+id)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	other := openQuestion("sig_3", "br_1", "unrelated")
	other.RequestID = "hreq_other"
	if err := s.CreateSignal(other); err != nil {
		t.Fatalf("create sig_3: %v", err)
	}

	got, err := s.ListSignalsByRequestID("br_1", "hreq_abc")
	if err != nil {
		t.Fatalf("list by request id: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d signals for hreq_abc, want 2", len(got))
	}

	// A different session with the same request id must not bleed through.
	crossSession, err := s.ListSignalsByRequestID("br_2", "hreq_abc")
	if err != nil {
		t.Fatalf("list by request id: %v", err)
	}
	if len(crossSession) != 0 {
		t.Errorf("request ids must be scoped per session, got %d rows", len(crossSession))
	}

	empty, err := s.ListSignalsByRequestID("br_1", "")
	if err != nil {
		t.Fatalf("list by empty request id: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an empty request id must match nothing, got %d rows", len(empty))
	}
}
