package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// stubKanban stands in for kanban-store and records what the classifier wrote.
// The bodies are the whole point of these checks, so they are captured off the
// wire rather than re-derived from the values that went in.
type stubKanban struct {
	patchedBodies map[string]string // card id -> body sent by PatchCardBody
	createdBodies []string          // bodies sent by CreateCard, in order
	links         []string          // "entity_type=entity_ref" per AddCardLink
}

func newStubKanban(t *testing.T) (*kanbanclient.Client, *stubKanban) {
	t.Helper()
	rec := &stubKanban{patchedBodies: map[string]string{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		path := r.URL.Path

		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/api/cards/"):
			cardID := strings.TrimPrefix(path, "/api/cards/")
			body, _ := payload["body"].(string)
			rec.patchedBodies[cardID] = body
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/move"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/links"):
			et, _ := payload["entity_type"].(string)
			ref, _ := payload["entity_ref"].(string)
			rec.links = append(rec.links, et+"="+ref)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/cards"):
			body, _ := payload["body"].(string)
			rec.createdBodies = append(rec.createdBodies, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"item":{"id":"new-card-1"}}`))

		default:
			t.Errorf("stub kanban got an unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	kc := kanbanclient.New(srv.URL)
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{
		"Backlog": "col-backlog", "In Progress": "col-inprogress",
		"Blocked": "col-blocked", "Review": "col-review", "Done": "col-done",
	}
	return kc, rec
}

func testSession() session {
	return session{
		SessionID: "br_1784567616625538966",
		Harness:   "claudecode",
		Purpose:   "autoworker",
		State:     "idle",
	}
}

// The defect this file was written for. applyNew stamped the session id into
// the body and applyExisting rebuilt that body from the model's summary plus a
// trailer of its own that named no session — so the first state update erased
// the provenance the create had written. Measured on the live board 2026-08-07:
// 19 of the 21 open classifier todos whose body named no session had one in
// their previous noteboard revision.
func TestUpdatingACardKeepsTheSessionItCameFrom(t *testing.T) {
	kc, rec := newStubKanban(t)
	sess := testSession()

	applyExisting(kc, sess, &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: "still working on the resolver"},
	}})

	body, ok := rec.patchedBodies["card-1"]
	if !ok {
		t.Fatalf("no body was patched onto card-1; got %v", rec.patchedBodies)
	}
	if !strings.Contains(body, "session="+sess.SessionID) {
		t.Fatalf("an updated card body must still name the session it came from.\ngot: %q", body)
	}
	if !strings.Contains(body, "still working on the resolver") {
		t.Fatalf("the model's summary is missing from the updated body.\ngot: %q", body)
	}
}

// The create path was already correct; it is pinned so a later change to the
// shared trailer cannot fix one path by breaking the other.
func TestCreatingACardStampsTheSessionItCameFrom(t *testing.T) {
	kc, rec := newStubKanban(t)
	sess := testSession()

	applyNew(kc, sess, nil, &Report{NewTasks: []NewRec{
		{Title: "Port the resolver", State: "in_progress", Summary: "started tonight"},
	}})

	if len(rec.createdBodies) != 1 {
		t.Fatalf("want 1 created card, got %d", len(rec.createdBodies))
	}
	if !strings.Contains(rec.createdBodies[0], "session="+sess.SessionID) {
		t.Fatalf("a created card body must name the session it came from.\ngot: %q", rec.createdBodies[0])
	}
}

// The body line is an echo. The link is the join, and it is what any reader
// should actually resolve a card back to its session with — so losing the echo
// must never have been able to lose the provenance itself. This asserts the
// id-keyed link is written alongside, which is why the defect above was a
// display defect and not data loss.
func TestACreatedCardCarriesTheIDKeyedSessionLinkNotJustTheEcho(t *testing.T) {
	kc, rec := newStubKanban(t)
	sess := testSession()

	applyNew(kc, sess, nil, &Report{NewTasks: []NewRec{
		{Title: "Port the resolver", State: "in_progress", Summary: "started tonight"},
	}})

	want := cardLinkSession + "=" + sess.SessionID
	found := false
	for _, l := range rec.links {
		if l == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an id-keyed link %q among %v", want, rec.links)
	}
}

// A trailer is only a trailer if nothing follows it. applyExisting used to
// append the blocker AFTER its provenance line while applyNew put it before,
// so the same card said two different things depending on which path last
// touched it.
func TestTheProvenanceLineIsLastOnBothWritePaths(t *testing.T) {
	kc, rec := newStubKanban(t)
	sess := testSession()

	applyExisting(kc, sess, &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "blocked", Summary: "paused", Blocker: "waiting on a deploy"},
	}})
	applyNew(kc, sess, nil, &Report{NewTasks: []NewRec{
		{Title: "Something else", State: "blocked", Summary: "paused", Blocker: "waiting on a deploy"},
	}})

	for name, body := range map[string]string{
		"updated": rec.patchedBodies["card-1"],
		"created": rec.createdBodies[0],
	} {
		trailerAt := strings.Index(body, "classifier @ ")
		blockerAt := strings.Index(body, "**Blocker:**")
		if trailerAt < 0 || blockerAt < 0 {
			t.Fatalf("%s body is missing the trailer or the blocker: %q", name, body)
		}
		if blockerAt > trailerAt {
			t.Fatalf("%s body puts the blocker after the provenance trailer: %q", name, body)
		}
		if !strings.Contains(body, "session="+sess.SessionID) {
			t.Fatalf("%s body does not name the session: %q", name, body)
		}
	}
}

// Both actions render through one function, so the two lines cannot drift into
// naming different things again. This pins the shape rather than the wording.
func TestBothProvenanceActionsRenderTheSameFields(t *testing.T) {
	for _, action := range []string{"created by", "updated by"} {
		line := provenanceTrailer(action, "br_1", "done")
		for _, want := range []string{action, "session=br_1", "state=done"} {
			if !strings.Contains(line, want) {
				t.Fatalf("provenanceTrailer(%q) = %q, missing %q", action, line, want)
			}
		}
	}
}
