package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUnreachableAccountIsRecordedWhileOthersStillFile reproduces the outage
// that motivated the whole degradations file.
//
// imap-pretender's IMAP host timed out for roughly two days. The command did the
// right thing locally — skipped that account so Gmail triage kept working — and
// then exited 0, so all 100 runs in scheduler history read `success` and nothing
// surfaced that a whole mail account had gone dark.
//
// Both halves are asserted here, because either alone is the wrong fix: the
// healthy account must still be walked, AND the run must end up failing.
func TestUnreachableAccountIsRecordedWhileOthersStillFile(t *testing.T) {
	var healthyWalked bool

	mailstack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.URL.Query().Get("account")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/folders"):
			if account == "broken-account" {
				// Exactly what mailstack returned during the real outage.
				http.Error(w, `{"error":"imap dial mail.example.com:993: i/o timeout"}`, http.StatusBadGateway)
				return
			}
			w.Write([]byte(`{"folders":[{"id":"INBOX","canonical":"inbox"}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/messages"):
			if account == "healthy-account" {
				healthyWalked = true
			}
			w.Write([]byte(`{"messages":[],"next_cursor":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mailstack.Close()

	accounts := []mailAccount{
		{ID: "broken-account", Enabled: true},
		{ID: "healthy-account", Enabled: true},
	}
	state := &boardState{
		Cards:            map[string]*cardState{},
		FiledLocators:    map[string]bool{},
		CardsByMessageID: map[string][]string{},
		CardsBySender:    map[string][]string{},
	}
	var degraded degradations

	_, err := collectUnfiled(
		config{MaxEmails: maxEmailsPerTick},
		newMailClient(mailstack.URL, "test-token"),
		accounts, state, &degraded,
	)
	if err != nil {
		t.Fatalf("collectUnfiled returned a hard error: %v", err)
	}

	if !healthyWalked {
		t.Error("the healthy account was not walked; one bad account must not stop the others")
	}
	if !degraded.any() {
		t.Fatal("an unreachable account produced no shortfall, so the run would exit 0 and read as success")
	}
	if got := degraded.exitCode(); got != 1 {
		t.Errorf("exit code %d, want 1 so the scheduler records `error`", got)
	}
	if joined := strings.Join(degraded.reasons, "; "); !strings.Contains(joined, "broken-account") {
		t.Errorf("shortfall %q does not name the account that failed", joined)
	}
}
