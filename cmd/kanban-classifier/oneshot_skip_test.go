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

// Oneshot sessions are model calls, not work to track: the sweep drops them by
// their purpose field and keeps everything else, discovered sessions included.
func TestSelectCandidates_SkipsOneshotCallsByPurpose(t *testing.T) {
	updated := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := summaryPage{}
		add := func(id, purpose, displayName string) {
			page.Sessions = append(page.Sessions, struct {
				SessionID   string `json:"sessionId"`
				State       string `json:"state"`
				Purpose     string `json:"purpose"`
				DisplayName string `json:"displayName"`
				Harness     string `json:"harness"`
				CreatedAt   string `json:"createdAt"`
				UpdatedAt   string `json:"updatedAt"`
			}{SessionID: id, State: "idle", Purpose: purpose, DisplayName: displayName, UpdatedAt: updated})
		}
		add("br_chat", msg.PurposeChat, "fix the deploy")
		add("br_oneshot", msg.PurposeOneshot, "fix the deploy")
		add("br_discovered", msg.PurposeDiscovered, "oneshot")
		add("br_dispatcher", msg.PurposeDispatcher, "card work")
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
	if strings.Join(ids, ",") != "br_chat,br_discovered,br_dispatcher" {
		t.Fatalf("candidates %v, want every session but the oneshot call", ids)
	}
}
