package rulestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testBoard      = "6e8b2c57-dfcd-4bec-9ad9-d92adbe2a830"
	testInProgress = "11111111-1111-4111-8111-111111111111"
	testDone       = "22222222-2222-4222-8222-222222222222"
	testAdmin      = "principal_000001"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// curatorDone is the curator's "completed, not an autoworker session" rule.
func curatorDone() string {
	return fmt.Sprintf(`{"conditions":[
		{"kind":"field_in","field":"state","values":["completed"]},
		{"kind":"field_not_in","field":"purpose","values":["autoworker"]},
		{"kind":"linked_card_in_column","board_id":%q,"column_id":%q}],
	 "effects":[{"kind":"move_linked_cards","board_id":%q,"to_column_id":%q}]}`, testBoard, testInProgress, testBoard, testDone)
}

func TestADefinitionOutsideTheVocabularyIsRefused(t *testing.T) {
	cases := map[string]string{
		"no conditions":        `{"conditions":[],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"no effects":           `{"conditions":[{"kind":"field_in","field":"state","values":["idle"]}],"effects":[]}`,
		"unknown kind":         `{"conditions":[{"kind":"field_matches","field":"state","values":["idle"]}],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"unknown field":        `{"conditions":[{"kind":"field_in","field":"principal_id","values":["x"]}],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"misspelled key":       `{"conditions":[{"kind":"field_in","feild":"state","values":["idle"]}],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"field it ignores":     `{"conditions":[{"kind":"field_in","field":"state","values":["idle"],"age":"1h"}],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"bad age":              `{"conditions":[{"kind":"updated_before","age":"two days"}],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"column by name":       `{"conditions":[{"kind":"linked_card_in_column","board_id":"` + testBoard + `","column_id":"In Progress"}],"effects":[{"kind":"add_session_tag","tag":"x"}]}`,
		"move with no chooser": `{"conditions":[{"kind":"field_in","field":"state","values":["completed"]}],"effects":[{"kind":"move_linked_cards","board_id":"` + testBoard + `","to_column_id":"` + testDone + `"}]}`,
	}
	for name, raw := range cases {
		if _, err := ParseDefinition(SourceSession, json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := ParseDefinition(SourceSession, json.RawMessage(curatorDone())); err != nil {
		t.Fatalf("the curator rule must parse: %v", err)
	}
}

func TestARuleIsActivatedInAKnownModeWithAReasonAndRetiringIsFinal(t *testing.T) {
	s := newTestStore(t)
	rule, err := s.CreateRule("close completed cards", "", SourceSession, testAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule("x", "", SourceSession, "slava"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a name where a principal id belongs must be refused, got %v", err)
	}
	if _, err := s.Activate(rule.ID, 1, ModeShadow, testAdmin, "try"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("activating a revision that does not exist must be refused, got %v", err)
	}
	if _, err := s.AddRevision(rule.ID, json.RawMessage(curatorDone()), "first", testAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(rule.ID, 1, "loud", testAdmin, "go"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "shadow, live") {
		t.Fatalf("an unknown mode must be refused naming the modes, got %v", err)
	}
	if _, err := s.Activate(rule.ID, 1, ModeShadow, testAdmin, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an activation without a reason must be refused, got %v", err)
	}
	active, err := s.Activate(rule.ID, 1, ModeShadow, testAdmin, "compare with kanban-curator")
	if err != nil {
		t.Fatal(err)
	}
	if active.State != RuleStateActive || active.Mode != ModeShadow || len(active.Activations) != 1 {
		t.Fatalf("unexpected rule after activation: %+v", active)
	}
	if _, err := s.Pause(rule.ID, testAdmin, "noisy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pause(rule.ID, testAdmin, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("pausing a paused rule must be a conflict, got %v", err)
	}
	if _, err := s.Retire(rule.ID, testAdmin, "replaced"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate(rule.ID, 1, ModeShadow, testAdmin, "back"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a retired rule must not come back, got %v", err)
	}
	if _, err := s.AddRevision(rule.ID, json.RawMessage(curatorDone()), "", testAdmin); !errors.Is(err, ErrConflict) {
		t.Fatalf("a retired rule takes no revision, got %v", err)
	}
}

func session(id, state, purpose, updatedAt string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"session_id":%q,"state":%q,"purpose":%q,"folder_name":"","updated_at":%q,"extra":{"kept":true}}`, id, state, purpose, updatedAt))
}

func TestAStateEpisodeAdvancesOnlyWhenTheStateChanges(t *testing.T) {
	s := newTestStore(t)
	observe := func(state string) Subject {
		t.Helper()
		stored, err := s.ObserveSubjects([]SubjectObservation{{Type: SubjectTypeSession, ID: "s1", State: state, Snapshot: session("s1", state, "", "2026-10-19T10:00:00Z")}})
		if err != nil {
			t.Fatal(err)
		}
		return stored[0]
	}
	if got := observe("idle").TriggerKey(); got != "idle#1" {
		t.Fatalf("first sight: %s", got)
	}
	if got := observe("idle").TriggerKey(); got != "idle#1" {
		t.Fatalf("a frame that changes nothing but updated_at must keep the episode: %s", got)
	}
	if got := observe("completed").TriggerKey(); got != "completed#2" {
		t.Fatalf("a state change starts an episode: %s", got)
	}
	if got := observe("idle").TriggerKey(); got != "idle#3" {
		t.Fatalf("returning to a state is a new episode, so a rule may fire again: %s", got)
	}
	stored, err := s.GetSubject(SubjectTypeSession, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Snapshot), `"kept":true`) {
		t.Fatalf("the snapshot must be kept verbatim: %s", stored.Snapshot)
	}
}

// fakeFacts is kanban-store as a map, counting calls.
type fakeFacts struct {
	placements map[string][]CardPlacement
	tags       map[string][]string
	calls      int
}

func (f *fakeFacts) LinkedCardPlacements(_ context.Context, sessionID string) ([]CardPlacement, error) {
	f.calls++
	return f.placements[sessionID], nil
}

func (f *fakeFacts) SessionTags(_ context.Context, sessionID string) ([]string, error) {
	f.calls++
	return f.tags[sessionID], nil
}

func TestEvaluationMatchesTheCuratorRuleAndCallsKanbanOnlyWhenItMust(t *testing.T) {
	definition, err := ParseDefinition(SourceSession, json.RawMessage(curatorDone()))
	if err != nil {
		t.Fatal(err)
	}
	facts := &fakeFacts{placements: map[string][]CardPlacement{
		"done-session":  {{CardID: "card-a", BoardID: testBoard, ColumnID: testInProgress}, {CardID: "card-b", BoardID: testBoard, ColumnID: testDone}},
		"other-session": {{CardID: "card-c", BoardID: testBoard, ColumnID: testDone}},
	}}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	subject := func(id, state, purpose string) Subject {
		return Subject{Type: SubjectTypeSession, ID: id, State: state, StateEpisode: 1, Snapshot: session(id, state, purpose, "2026-10-19T11:00:00Z")}
	}

	idle, err := EvaluateSession(context.Background(), definition, subject("done-session", "idle", ""), at, facts)
	if err != nil || idle.Matched || facts.calls != 0 {
		t.Fatalf("an idle session must not match, and must cost no kanban call: matched=%v calls=%d err=%v", idle.Matched, facts.calls, err)
	}
	autoworker, err := EvaluateSession(context.Background(), definition, subject("done-session", "completed", "autoworker"), at, facts)
	if err != nil || autoworker.Matched || facts.calls != 0 {
		t.Fatalf("an autoworker session is another rule's: matched=%v calls=%d err=%v", autoworker.Matched, facts.calls, err)
	}
	match, err := EvaluateSession(context.Background(), definition, subject("done-session", "completed", "chat"), at, facts)
	if err != nil || !match.Matched {
		t.Fatalf("want a match: %+v %v", match, err)
	}
	if len(match.PlannedEffects) != 1 || match.PlannedEffects[0].Path != "/api/cards/card-a/move" {
		t.Fatalf("only the card in the condition's column is moved, not the one already in Done: %+v", match.PlannedEffects)
	}
	other, err := EvaluateSession(context.Background(), definition, subject("other-session", "completed", "chat"), at, facts)
	if err != nil || other.Matched {
		t.Fatalf("a session whose card already left In Progress must not match: %+v %v", other, err)
	}
}

func TestUpdatedBeforeAndTagConditions(t *testing.T) {
	definition, err := ParseDefinition(SourceSession, json.RawMessage(`{"conditions":[
		{"kind":"field_in","field":"state","values":["completed"]},
		{"kind":"updated_before","age":"30m"},
		{"kind":"session_tag_absent","tag":"review-settled"}],
		"effects":[{"kind":"add_session_tag","tag":"review-settled"},{"kind":"send_session_message","text":"review your work"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	facts := &fakeFacts{tags: map[string][]string{"settled": {"review-settled"}}}
	subject := func(id, updatedAt string) Subject {
		return Subject{Type: SubjectTypeSession, ID: id, State: "completed", StateEpisode: 1, Snapshot: session(id, "completed", "autoworker", updatedAt)}
	}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	recent, _ := EvaluateSession(context.Background(), definition, subject("fresh", "2026-10-19T11:45:00Z"), at, facts)
	old, _ := EvaluateSession(context.Background(), definition, subject("fresh", "2026-10-19T11:00:00Z"), at, facts)
	settled, _ := EvaluateSession(context.Background(), definition, subject("settled", "2026-10-19T11:00:00Z"), at, facts)
	if recent.Matched || !old.Matched || settled.Matched {
		t.Fatalf("recent=%v old=%v settled=%v; want false true false", recent.Matched, old.Matched, settled.Matched)
	}
	if len(old.PlannedEffects) != 2 || old.PlannedEffects[1].Path != "/sessions/fresh/send" {
		t.Fatalf("effects in order: %+v", old.PlannedEffects)
	}
}

func TestALinkedCardOnNoBoardHasNoPlacement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/entities/session/s1/cards":
			w.Write([]byte(`[{"card_id":"detached"},{"card_id":"placed"}]`))
		case "/api/cards/detached":
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case "/api/cards/placed":
			w.Write([]byte(`{"placements":[{"board_id":"b","column_id":"c"}]}`))
		case "/api/cards/broken":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer server.Close()
	kanban := NewKanbanStore(server.URL, "token")
	placements, err := kanban.LinkedCardPlacements(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 1 || placements[0].CardID != "placed" {
		t.Fatalf("placements %+v", placements)
	}
	var card CardDetail
	if err := kanban.getJSON(context.Background(), "/api/cards/broken", &card); err == nil || errors.Is(err, errKanbanNotFound) {
		t.Fatalf("a 500 must stay an error, got %v", err)
	}
}

func TestStatusReasonIsReadFromTheSessionStatus(t *testing.T) {
	definition, err := ParseDefinition(SourceSession, json.RawMessage(`{"conditions":[
		{"kind":"field_in","field":"state","values":["completed"]},
		{"kind":"field_in","field":"status_reason","values":["idle_reaped"]}],
		"effects":[{"kind":"add_session_tag","tag":"reaped-after-finishing"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	reaped := Subject{Type: SubjectTypeSession, ID: "s", State: "completed", StateEpisode: 1,
		Snapshot: json.RawMessage(`{"session_id":"s","state":"completed","updated_at":"2026-10-19T11:00:00Z","status":{"state":"completed","reason":"idle_reaped"}}`)}
	marked := Subject{Type: SubjectTypeSession, ID: "s", State: "completed", StateEpisode: 1,
		Snapshot: json.RawMessage(`{"session_id":"s","state":"completed","updated_at":"2026-10-19T11:00:00Z","status":{"state":"completed","reason":"mark_done_user"}}`)}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	first, _ := EvaluateSession(context.Background(), definition, reaped, at, &fakeFacts{})
	second, _ := EvaluateSession(context.Background(), definition, marked, at, &fakeFacts{})
	if !first.Matched || second.Matched {
		t.Fatalf("reaped matched=%v, marked done matched=%v; want true false", first.Matched, second.Matched)
	}
}

func TestColumnCardsReadsEveryPageAndRefusesAnOversizedBody(t *testing.T) {
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/columns/col/cards":
			served++
			offset := r.URL.Query().Get("offset")
			cards := `[{"item":{"id":"a"},"placement":{"updated_at":"2026-10-20T10:00:00Z"},"links":[{"entity_type":"session","entity_ref":"s1"},{"entity_type":"email","entity_ref":"x"}]}]`
			if offset != "0" {
				cards = `[{"item":{"id":"b"},"placement":{"updated_at":"2026-10-20T11:00:00Z"},"links":[]}]`
			}
			fmt.Fprintf(w, `{"column":{"board_id":"board"},"total":%d,"cards":%s}`, columnPageSize+1, cards)
		case "/api/cards/huge":
			w.Write([]byte(`{"pad":"` + strings.Repeat("x", kanbanResponseLimit) + `"}`))
		}
	}))
	defer server.Close()
	kanban := NewKanbanStore(server.URL, "token")
	arrivals, err := kanban.ColumnCards(context.Background(), "board", "col")
	if err != nil {
		t.Fatal(err)
	}
	if len(arrivals) != 2 || served != 2 || len(arrivals[0].SessionIDs) != 1 || arrivals[0].SessionIDs[0] != "s1" {
		t.Fatalf("arrivals %+v after %d pages", arrivals, served)
	}
	if _, err := kanban.ColumnCards(context.Background(), "another-board", "col"); err == nil {
		t.Fatal("a column on another board must be refused")
	}
	if _, err := kanban.Card(context.Background(), "huge"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized body must say so, got %v", err)
	}
}
