package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
			page.Sessions = append(page.Sessions, struct {
				SessionID   string `json:"sessionId"`
				State       string `json:"state"`
				Purpose     string `json:"purpose"`
				DisplayName string `json:"displayName"`
				Harness     string `json:"harness"`
				CreatedAt   string `json:"createdAt"`
				UpdatedAt   string `json:"updatedAt"`
			}{SessionID: id, State: "idle", UpdatedAt: now.Add(-age).Format(time.RFC3339)})
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

func TestSignatures(t *testing.T) {
	cur := buildSig(4064, "idle", "2026-09-16T21:23:57Z")
	if cur != "kanban-check-sig:events=4064:state=idle:t=202609162123" {
		t.Fatalf("sig %q", cur)
	}
	if !sigUnchanged(cur, cur) {
		t.Error("identical signature reported changed")
	}
	if sigUnchanged("", cur) {
		t.Error("no prior signature reported unchanged")
	}
	if !sigUnchanged("kanban-check-sig:msg=37:state=idle:t=202609162123", cur) {
		t.Error("an old msg= signature with the same state and minute must count as unchanged")
	}
	if sigUnchanged("kanban-check-sig:msg=37:state=running:t=202609162123", cur) {
		t.Error("an old signature with another state must count as changed")
	}
	if sigUnchanged("kanban-check-sig:events=4000:state=idle:t=202609162123", cur) {
		t.Error("a new-format signature with another count must count as changed")
	}

	n, known := priorEventCount(map[string]bool{cur: true})
	if !known || n != 4064 {
		t.Errorf("priorEventCount = %d %v", n, known)
	}
	if _, known := priorEventCount(map[string]bool{"kanban-check-sig:msg=37:state=idle:t=1": true}); known {
		t.Error("an old msg= signature has no event count")
	}
}
