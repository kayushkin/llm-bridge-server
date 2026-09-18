package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The candidate walk pages the summary and stops at the first session older than
// the cutoff, instead of downloading every session to filter locally.
func TestListSessionsUpdatedSince_StopsAtTheCutoff(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.String())
		if r.URL.Path != "/sessions/summary" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		page := summaryPage{}
		add := func(id string, age time.Duration) {
			page.Sessions = append(page.Sessions, summaryRow{SessionID: id, State: "idle", UpdatedAt: now.Add(-age).Format(time.RFC3339)})
		}
		switch r.URL.Query().Get("before") {
		case "":
			add("new-1", time.Hour)
			add("new-2", 2*time.Hour)
			page.Next = "cursor-1"
		case "cursor-1":
			add("new-3", 3*24*time.Hour)
			add("old-1", 8*24*time.Hour) // past the cutoff: the walk ends here
			page.Next = "cursor-2"
		default:
			t.Fatalf("walked past the cutoff: %s", r.URL)
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	got, err := listSessionsUpdatedSince(srv.URL, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	if strings.Join(ids, ",") != "new-1,new-2,new-3" {
		t.Fatalf("candidates %v", ids)
	}
	if len(requests) != 2 || !strings.Contains(requests[0], "limit=100") {
		t.Fatalf("requests %v", requests)
	}
}

// A session page is always bounded and asks for tool payload previews.
func TestGetSessionPage_IsBounded(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		fmt.Fprint(w, `{"model":{"turns":[],"entries":{},"validator":{"eventCount":42}}}`)
	}))
	defer srv.Close()
	page, err := getSessionPage(srv.URL, "br_1")
	if err != nil {
		t.Fatal(err)
	}
	if seen != fmt.Sprintf("/sessions/br_1/messages?limit=%d&payload=preview", digestTurns) {
		t.Fatalf("request %q", seen)
	}
	if page.eventCount != 42 {
		t.Fatalf("eventCount %d", page.eventCount)
	}
}

func TestDigestMessages_FlattensATurnPage(t *testing.T) {
	var page turnPage
	if err := json.Unmarshal([]byte(`{"model":{
		"turns":[{"entryIds":["e1","e2","e3","e4","e5"]}],
		"entries":{
			"e1":{"role":"user","kind":"text","text":"fix the bug","ts":"t1"},
			"e2":{"role":"user","kind":"text","text":"fix the bug","duplicate":true},
			"e3":{"role":"assistant","kind":"tool_call","toolName":"Bash","ts":"t2"},
			"e4":{"role":"tool","kind":"tool_result"},
			"e5":{"role":"assistant","kind":"result","text":"fixed","ts":"t3"}
		}}}`), &page); err != nil {
		t.Fatal(err)
	}
	msgs := digestMessages(page)
	if len(msgs) != 3 {
		t.Fatalf("messages %+v", msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "fix the bug" {
		t.Errorf("prompt %+v", msgs[0])
	}
	if msgs[1].Events[0]["tool_name"] != "Bash" {
		t.Errorf("tool call %+v", msgs[1])
	}
	if msgs[2].Role != "assistant" || msgs[2].Content != "fixed" {
		t.Errorf("result %+v", msgs[2])
	}
	if !strings.Contains(buildToolActivitySection(msgs), "- Bash") {
		t.Error("tool activity section does not see the flattened tool call")
	}
}

// One model call per finished response: the signature names the result entry
// that closed it, and only a signature naming the same entry says "already read".
func TestOneClassificationPerFinishedResponse(t *testing.T) {
	sig := buildSig("e_2729128", 8328)
	if sig != "kanban-check-sig:result=e_2729128:events=8328" {
		t.Fatalf("sig %q", sig)
	}
	if !responseAlreadyClassified(sig, "e_2729128") {
		t.Error("the response the signature names was reported unread")
	}
	if responseAlreadyClassified(sig, "e_2730001") {
		t.Error("a newer finished response was reported already classified")
	}
	if responseAlreadyClassified("", "e_2729128") {
		t.Error("no prior signature reported already classified")
	}
	if responseAlreadyClassified(sig, "") {
		t.Error("no finished response can never be already classified")
	}
	if responseAlreadyClassified("kanban-check-sig:garbage", "e_2729128") {
		t.Error("an unreadable prior signature reported already classified")
	}
	if responseAlreadyClassified("kanban-check-sig:result=:events=5", "e_2729128") {
		t.Error("a signature with an empty result reported already classified")
	}

	n, known := priorEventCount(map[string]bool{sig: true})
	if !known || n != 8328 {
		t.Errorf("priorEventCount = %d %v", n, known)
	}
}

// llm-bridge appends events to a session after its turn has ended, and a session
// moves between idle, awaiting_user and completed with nothing new said. Neither
// is a new response, so neither may buy another model call.
func TestEventsAndStateChangesAfterTheTurnAreNotANewResponse(t *testing.T) {
	classifiedAt := buildSig("e_2729128", 8328)
	if buildSig("e_2729128", 8331) == classifiedAt {
		t.Fatal("the control is broken: a later event count must give another signature")
	}
	if !responseAlreadyClassified(classifiedAt, "e_2729128") {
		t.Error("three more events on the same finished response: reported unread")
	}
}

// Signatures from before 2026-09-18 name no response, so they cannot say this one
// was read. Such a session is classified once more and then carries one that can.
func TestASignatureThatNamesNoResponseIsNotAlreadyClassified(t *testing.T) {
	for _, old := range []string{
		"kanban-check-sig:events=4064:state=idle:t=202609162123",
		"kanban-check-sig:msg=37:state=idle:t=202609162123",
	} {
		if responseAlreadyClassified(old, "e_2729128") {
			t.Errorf("%s reported already classified", old)
		}
	}
	// The event count of the 2026-09-16 format still re-opens a do-not-track session.
	n, known := priorEventCount(map[string]bool{"kanban-check-sig:events=4064:state=idle:t=202609162123": true})
	if !known || n != 4064 {
		t.Errorf("priorEventCount = %d %v", n, known)
	}
	if _, known := priorEventCount(map[string]bool{"kanban-check-sig:msg=37:state=idle:t=1": true}); known {
		t.Error("an old msg= signature has no event count")
	}
}

// The final output of a turn is its result entry. A turn cut short has none, a
// /compact leaves an empty one, and the OTel copy is a duplicate.
func TestNewestFinishedResponseID(t *testing.T) {
	var page turnPage
	if err := json.Unmarshal([]byte(`{"model":{
		"turns":[
			{"entryIds":["e_1","e_2"]},
			{"entryIds":["e_3","e_4","e_5"]},
			{"entryIds":["e_6","e_7"]},
			{"entryIds":["e_8","e_9"]}],
		"entries":{
			"e_1":{"role":"user","kind":"text","text":"fix it"},
			"e_2":{"role":"assistant","kind":"result","text":"fixed"},
			"e_3":{"role":"user","kind":"text","text":"and the other one"},
			"e_4":{"role":"assistant","kind":"text","text":"working"},
			"e_5":{"role":"assistant","kind":"result","text":"both fixed"},
			"e_6":{"role":"user","kind":"text","text":"/compact"},
			"e_7":{"role":"assistant","kind":"result","text":""},
			"e_8":{"role":"user","kind":"text","text":"one more"},
			"e_9":{"role":"assistant","kind":"tool_call","toolName":"Bash"}}}}`), &page); err != nil {
		t.Fatal(err)
	}
	if got := newestFinishedResponseID(page); got != "e_5" {
		t.Errorf("newest finished response %q, want e_5: the compact answered nothing and the last turn has not ended", got)
	}

	page.Model.Entries["e_10"] = page.Model.Entries["e_5"]
	duplicate := page.Model.Entries["e_10"]
	duplicate.Duplicate = true
	page.Model.Entries["e_10"] = duplicate
	page.Model.Turns[3].EntryIDs = append(page.Model.Turns[3].EntryIDs, "e_10")
	if got := newestFinishedResponseID(page); got != "e_5" {
		t.Errorf("a hidden duplicate was read as a finished response: %q", got)
	}

	if got := newestFinishedResponseID(turnPage{}); got != "" {
		t.Errorf("an empty page has a finished response: %q", got)
	}
}

// A turn that has not ended has no final output. The list of active states is
// llm-bridge's; a permission prompt parks a session mid-turn, and awaiting_user is
// a turn that ended.
func TestTurnInFlight(t *testing.T) {
	for _, state := range []msg.SessionState{
		msg.SessionStarting, msg.SessionModelGenerating, msg.SessionToolRunning, msg.SessionCompacting,
		msg.SessionBackgroundTasksRunning, msg.SessionRateLimited, msg.SessionRunning,
		msg.SessionAwaitingPermission, msg.SessionWaitingApproval,
	} {
		if !turnInFlight(state) {
			t.Errorf("%s: a turn is in flight", state)
		}
	}
	for _, state := range []msg.SessionState{
		msg.SessionIdle, msg.SessionAwaitingUser, msg.SessionCompleted, msg.SessionError,
		msg.SessionAborted, msg.SessionDisconnected, msg.SessionPaused,
	} {
		if turnInFlight(state) {
			t.Errorf("%s: no turn is in flight", state)
		}
	}
}

func TestNewestMessageTime(t *testing.T) {
	newest, err := newestMessageTime([]bridgeMessage{
		{Timestamp: "2026-09-16T21:00:00Z"}, {Timestamp: ""}, {Timestamp: "2026-09-16T21:05:00Z"}, {Timestamp: "2026-09-16T21:01:00Z"},
	})
	if err != nil || !newest.Equal(time.Date(2026, 9, 16, 21, 5, 0, 0, time.UTC)) {
		t.Errorf("newest = %v, %v", newest, err)
	}
	if newest, err := newestMessageTime(nil); err != nil || !newest.IsZero() {
		t.Errorf("no messages: %v, %v", newest, err)
	}
	if _, err := newestMessageTime([]bridgeMessage{{Timestamp: "t1"}}); err == nil {
		t.Error("an unparseable timestamp must be an error, not a missing time")
	}
}

func TestTheLookbackMeasuresTheConversation(t *testing.T) {
	cutoff := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	if !saidNothingSince(time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC), cutoff) {
		t.Error("a session last spoken in before the cutoff is inside the lookback")
	}
	if saidNothingSince(cutoff, cutoff) || saidNothingSince(cutoff.Add(time.Hour), cutoff) {
		t.Error("a session spoken in at or after the cutoff is outside the lookback")
	}
	if saidNothingSince(time.Time{}, cutoff) {
		t.Error("a page with no entry time was judged stale")
	}
}
