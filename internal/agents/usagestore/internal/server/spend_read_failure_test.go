package server

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// A failed store read inside assembleProvider used to be invisible. Three of
// the six reads discarded their error into `_`, so an unreadable spend_daily
// table answered 200 with every figure at zero — indistinguishable from an
// account that has spent nothing. Card 0e5d003c measured that; these tests pin
// the part of it that was never a decision: every failed read says so.
//
// The response SHAPE is deliberately not asserted here. Whether a failed read
// should 500, render null, or carry an `errors` list is the open question the
// card reserves for the user, and a test that pinned any of those answers
// would have to be deleted when they choose.

// captureLog redirects the standard logger for one test and hands back what was
// written to it. It restores the previous writer and flags, so a failure here
// cannot silence the logger for the rest of the package.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return &buf
}

// unreadableSpendServer returns a server whose store is closed, so every read
// it makes fails. This is measurement (a) from card 0e5d003c.
func unreadableSpendServer(t *testing.T) *Server {
	t.Helper()
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "k1", "primary")
	saveSpend(t, store, "anthropic", "k1", daysAgoDate(0), 250)
	if err := store.Close(); err != nil {
		t.Fatalf("Close store: %v", err)
	}
	return srv
}

func TestEachFailedPerKeyTotalsWindowIsNamedInTheLog(t *testing.T) {
	logged := captureLog(t)
	srv := unreadableSpendServer(t)

	do(t, srv, "GET", "/api/usage/spend/keys", "")

	// One line per window, because one window can fail while the others
	// succeed and the reader has to know WHICH figure is untrustworthy.
	for _, window := range []string{"24h", "7d", "30d"} {
		want := "[spend] per-key totals anthropic " + window + ":"
		if !strings.Contains(logged.String(), want) {
			t.Errorf("no log line for the failed %s window.\nwant a line containing: %s\ngot:\n%s",
				window, want, logged.String())
		}
	}
}

// The three windows are read for every provider, so a failure has to name the
// provider too — otherwise six lines from two providers are six lines about
// nothing in particular.
func TestAFailedPerKeyTotalsReadNamesItsProvider(t *testing.T) {
	logged := captureLog(t)
	srv := unreadableSpendServer(t)

	do(t, srv, "GET", "/api/usage/spend/keys", "")

	for _, provider := range []string{"anthropic", "openai"} {
		if !strings.Contains(logged.String(), "[spend] per-key totals "+provider+" ") {
			t.Errorf("no per-key-totals failure line naming provider %q.\ngot:\n%s",
				provider, logged.String())
		}
	}
}

// The underlying error must reach the line. A log line that says a read failed
// without saying how is the same dead end as no line at all.
//
// The assertion is per-LINE on purpose. The sibling reads (list meta, list
// topups) already log the identical "database is closed" text, so a
// whole-buffer Contains is satisfied by their lines and cannot detect the
// per-key-totals line dropping the error. It scored SURVIVED against exactly
// that mutation before this was narrowed.
func TestAFailedPerKeyTotalsReadCarriesTheUnderlyingError(t *testing.T) {
	logged := captureLog(t)
	srv := unreadableSpendServer(t)

	do(t, srv, "GET", "/api/usage/spend/keys", "")

	var perKeyLines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.HasPrefix(line, "[spend] per-key totals ") {
			perKeyLines = append(perKeyLines, line)
		}
	}
	if len(perKeyLines) == 0 {
		t.Fatalf("no per-key-totals line at all to carry an error.\ngot:\n%s", logged.String())
	}
	for _, line := range perKeyLines {
		if !strings.Contains(line, "database is closed") {
			t.Errorf("a per-key-totals failure line does not carry the store's own error: %q", line)
		}
	}
}

// A healthy read must stay quiet, or the line means nothing when it appears.
func TestAHealthySpendReadLogsNoFailure(t *testing.T) {
	logged := captureLog(t)
	srv, store := newSpendServer(t)
	saveMeta(t, store, "anthropic", "k1", "primary")
	saveSpend(t, store, "anthropic", "k1", daysAgoDate(0), 250)

	do(t, srv, "GET", "/api/usage/spend/keys", "")

	if strings.Contains(logged.String(), "[spend] per-key totals") {
		t.Errorf("a healthy read logged a failure:\n%s", logged.String())
	}
}
