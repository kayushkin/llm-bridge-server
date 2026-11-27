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

// The lookback is seven days, and it has two failure directions. Widened, a
// tick pays to read sessions nobody is working on. Narrowed, a session still
// being worked on drops off the board and the tick reads quiet, which looks
// the same as a week with nothing to do. TestListSessionsUpdatedSince_StopsAtTheCutoff
// passes its own cutoff, so nothing held the number selectCandidates uses.
//
// Written 2026-09-25 for the quiet-side row ported from 3aa6a3fc, which the
// branch's candidates_test.go caught against the old selectCandidates. The ages
// are literals, a day clear of the bound on each side, so the test moves with
// neither the constant nor the clock.
func TestTheCandidateLookbackKeepsSixDaysAndDropsEight(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := summaryPage{}
		add := func(id string, age time.Duration) {
			page.Sessions = append(page.Sessions, summaryRow{
				SessionID: id, State: "idle", Type: string(msg.SessionTypeInteractive), Purpose: msg.PurposeChat,
				UpdatedAt: now.Add(-age).UTC().Format(time.RFC3339),
			})
		}
		add("br_six_days", 6*24*time.Hour)
		add("br_eight_days", 8*24*time.Hour)
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	got, err := selectCandidates(config{BridgeURL: srv.URL})
	if err != nil {
		t.Fatalf("selectCandidates: %v", err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	if strings.Join(ids, ",") != "br_six_days" {
		t.Fatalf("candidates = %v, want only br_six_days — the lookback is seven days: a session "+
			"touched six days ago is still being worked on, and one touched eight days ago is not", ids)
	}
}
