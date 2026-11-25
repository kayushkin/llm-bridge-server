package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The sweep reads the sessions a person drives and nothing else. It matches on
// the type the bridge assigned: a oneshot model call named like a chat, this
// command's own included, stays out, and so does every autonomous, system,
// herald and external session, and a row with no type at all.
func TestSelectCandidates_KeepsInteractiveSessionsOnly(t *testing.T) {
	updated := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := summaryPage{}
		add := func(id string, sessionType msg.SessionType, purpose, displayName string) {
			page.Sessions = append(page.Sessions, summaryRow{
				SessionID: id, State: "idle", Type: string(sessionType), Purpose: purpose,
				DisplayName: displayName, UpdatedAt: updated,
			})
		}
		add("br_chat", msg.SessionTypeInteractive, msg.PurposeChat, "fix the deploy")
		add("br_own_call", msg.SessionTypeExternal, msg.PurposeOneshot, "[SESSION META] bridge_id: br_chat")
		add("br_oneshot_named_like_a_chat", msg.SessionTypeExternal, msg.PurposeOneshot, "fix the deploy")
		add("br_discovered", msg.SessionTypeExternal, msg.PurposeDiscovered, "hello")
		add("br_dispatcher", msg.SessionTypeAutonomous, msg.PurposeDispatcher, "card work")
		add("br_renamer", msg.SessionTypeSystem, "renamer", "rename br_chat")
		add("herald-1", msg.SessionTypeHerald, "herald", "reminder")
		add("br_no_type", "", msg.PurposeChat, "fix the deploy")
		add("br_second_chat", msg.SessionTypeInteractive, msg.PurposeChat, "kanban tag rules UI")
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	got, err := selectCandidates(config{BridgeURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	if strings.Join(ids, ",") != "br_chat,br_second_chat" {
		t.Fatalf("candidates %v, want the two interactive sessions and nothing else", ids)
	}
}

func TestInteractiveSessionsOnly_CountsWhatItDropsByType(t *testing.T) {
	_, skipped := interactiveSessionsOnly([]session{
		{SessionID: "a", Type: "interactive"},
		{SessionID: "b", Type: "external"},
		{SessionID: "c", Type: "external"},
		{SessionID: "d", Type: "autonomous"},
		{SessionID: "e", Type: ""},
	})
	if got := describeSkippedTypes(skipped); got != "(no type)=1 autonomous=1 external=2" {
		t.Errorf("skipped %q", got)
	}
	if got := describeSkippedTypes(map[string]int{}); got != "none" {
		t.Errorf("nothing skipped reads %q", got)
	}
}
