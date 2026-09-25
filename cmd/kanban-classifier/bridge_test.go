package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every session this binary looks at arrives through these three functions:
// listSessionsUpdatedSince, getSessionPage and getSession. getSession is only
// reached through `-session <id>`, the flag path an operator uses to classify
// one session by hand. Ported 2026-09-25 from 348e0589, where the first two were
// still listSessions and getSessionMessages.
//
// The shape they share is the one that matters. Each checks a status and then
// decodes. Delete the status check and the call still fails, one step later, on
// the decode — so a test asserting only that an error came back is green on a
// client that has stopped reading status codes, and the error then blames the
// body for a request that was refused. Every assertion below is on the status
// reaching the message.

// serveBridge answers one status and one body on every path, and records which
// path it was asked for. Answering everything is deliberate here: these tests
// are about what the client does with an answer, and the route each one uses is
// asserted separately below.
func serveBridge(t *testing.T, status int, body string) (url string, asked *string) {
	t.Helper()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &path
}

func TestGetSessionNamesTheStatusRatherThanBlamingTheBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		// A well-formed session object with a failing status. A 500 carrying
		// an error page is caught by the JSON decode whether the status guard
		// exists or not, so it cannot tell the guard from its absence; this
		// one decodes cleanly into a usable session, which is what would then
		// be classified and billed for.
		{name: "a bridge answering 500 with a valid session", status: 500,
			body: `{"session_id":"s1","state":"completed"}`, want: "500"},
		{name: "a session that does not exist", status: 404,
			body: `{"error":"no such session"}`, want: "404"},
		{name: "a body that is not a session object", status: 200,
			body: `["not","an","object"]`, want: "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := serveBridge(t, tc.status, tc.body)
			sess, err := getSession(url, "s1")
			if err == nil {
				t.Fatalf("want an error, got %+v", sess)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not carry %q", err, tc.want)
			}
		})
	}
}

// The id is what every later write keys on — the check signature, the session
// card link, the do-not-track tag. A bridge that answers without one must not
// leave the classifier holding a session whose id is the empty string, because
// every one of those writes would then land on the entity `session/`.
func TestASessionAnsweredWithoutAnIDKeepsTheOneItWasAskedFor(t *testing.T) {
	url, asked := serveBridge(t, 200, `{"state":"completed","display_name":"a run"}`)

	sess, err := getSession(url, "sess-42")
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	if sess.SessionID != "sess-42" {
		t.Errorf("session_id = %q, want the id asked for; every write this binary makes "+
			"keys on it", sess.SessionID)
	}
	if sess.State != "completed" {
		t.Errorf("state = %q, want the decoded value", sess.State)
	}
	if *asked != "/sessions/sess-42" {
		t.Errorf("asked for %q, want /sessions/sess-42", *asked)
	}
}

// And the other direction: an id the bridge DID supply must win, because the
// bridge is the source of truth for its own ids and the caller's argument may
// be an alias.
func TestASessionAnsweredWithAnIDKeepsTheBridgesOwn(t *testing.T) {
	url, _ := serveBridge(t, 200, `{"session_id":"canonical","state":"idle"}`)

	sess, err := getSession(url, "whatever-was-asked")
	if err != nil {
		t.Fatalf("getSession: %v", err)
	}
	if sess.SessionID != "canonical" {
		t.Errorf("session_id = %q, want the bridge's own", sess.SessionID)
	}
}

func TestListingSessionsNamesTheStatusRatherThanBlamingTheBody(t *testing.T) {
	url, asked := serveBridge(t, 500, `{"sessions":[]}`)

	if _, err := listSessionsUpdatedSince(url, time.Time{}); err == nil {
		t.Fatal("a 500 answered with a well-formed empty page was read as a box with no " +
			"sessions on it, which is a tick that classifies nothing and reports success")
	} else if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not carry the status", err)
	}
	if *asked != "/sessions/summary" {
		t.Errorf("asked for %q, want /sessions/summary", *asked)
	}
}

func TestReadingASessionsTurnsNamesTheStatusRatherThanBlamingTheBody(t *testing.T) {
	// The expensive direction. An unreadable transcript read as an empty one
	// sends the model a digest with no conversation in it and pays for the
	// call, and the answer is a card describing a session nobody looked at.
	url, asked := serveBridge(t, 500, `{"model":{"turns":[],"entries":{}}}`)

	if _, err := getSessionPage(url, "s1"); err == nil {
		t.Fatal("a 500 answered with a well-formed empty page was read as a session with " +
			"no messages, which is a paid classification of nothing")
	} else if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not carry the status", err)
	}
	if *asked != "/sessions/s1/messages" {
		t.Errorf("asked for %q, want /sessions/s1/messages", *asked)
	}
}

// A base URL with a trailing slash is what an environment variable copied out
// of a browser carries, and it must not produce a double slash in the path.
func TestATrailingSlashOnTheBridgeURLDoesNotDoubleTheSeparator(t *testing.T) {
	url, asked := serveBridge(t, 200, `{"sessions":[]}`)
	if _, err := listSessionsUpdatedSince(url+"/", time.Time{}); err != nil {
		t.Fatalf("listSessionsUpdatedSince: %v", err)
	}
	if *asked != "/sessions/summary" {
		t.Errorf("asked for %q, want /sessions/summary", *asked)
	}

	url, asked = serveBridge(t, 200, `{"model":{"turns":[],"entries":{}}}`)
	if _, err := getSessionPage(url+"/", "s1"); err != nil {
		t.Fatalf("getSessionPage: %v", err)
	}
	if *asked != "/sessions/s1/messages" {
		t.Errorf("asked for %q, want /sessions/s1/messages", *asked)
	}

	url, asked = serveBridge(t, 200, `{"session_id":"s1"}`)
	if _, err := getSession(url+"/", "s1"); err != nil {
		t.Fatalf("getSession: %v", err)
	}
	if *asked != "/sessions/s1" {
		t.Errorf("asked for %q, want /sessions/s1", *asked)
	}
}
