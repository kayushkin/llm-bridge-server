package kanbanclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubKanban stands in for kanban-store on the session-cards route.
//
// It routes the way kanban-store routes, rather than answering every request
// with the canned body. kanban-store's entityScoped handler splits
// r.URL.Path — the percent-DECODED path — on "/" and dispatches on the third
// segment, answering 404 {"error":"not found"} when that segment is not one it
// serves. A fixture that ignores the path cannot notice a request aimed at the
// wrong URL, and this client builds its URL from a caller-supplied id.
//
// Verified against the running kanban-store on :8305, because source is a
// claim: an unknown session ref returns 200 [], and the escaped path this
// client sends for an id containing a slash returns 404 {"error":"not found"}.
func stubKanban(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI is the raw request line. r.URL.Path would show the
		// server's percent-decoding of it, which is not what this asserts.
		gotPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")

		// kanban-store's routing, reproduced from internal/api/api.go. It
		// reads URL.Path, so Go has already turned any %2F back into a real
		// slash by this point and the segments shift.
		rest := strings.TrimPrefix(r.URL.Path, "/api/entities/")
		parts := strings.Split(rest, "/")
		if len(parts) < 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"expected /api/entities/:type/:ref/..."}`))
			return
		}
		if parts[2] != "cards" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), &gotPath
}

func TestLinkedTodoForSessionTakesTheFirstCard(t *testing.T) {
	// kanban-store returns links oldest-first, and the first is the todo the
	// session was created for. Re-sorting here would second-guess the store
	// that owns the ordering.
	//
	// That ordering is kanban-store's ListCardsByEntity —
	// ORDER BY MIN(created_at), card_id — and it is pinned on that side by
	// TestListCardsByEntityIsOldestLinkFirst. The tiebreak on card_id matters:
	// without a total order, two links written in the same clock tick would
	// come back in an arbitrary order and "the first card" would not be a
	// stable answer.
	client, path := stubKanban(t, http.StatusOK, `[
		{"card_id":"todo-oldest","item":{"id":"todo-oldest","title":"dispatch"}},
		{"card_id":"todo-newer","item":{"id":"todo-newer","title":"classified work"}}
	]`)

	got, err := client.LinkedTodoForSession(context.Background(), "br_1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "todo-oldest" {
		t.Errorf("todo = %q, want todo-oldest", got)
	}
	if want := "/api/entities/session/br_1/cards"; *path != want {
		t.Errorf("path = %q, want %q", *path, want)
	}
}

func TestLinkedTodoForSessionSkipsOrphanedLinks(t *testing.T) {
	// A null item is a link that outlived its noteboard item. Returning its
	// card id would point the signal at a todo no view can render.
	//
	// kanban-store produces that null in GetItems, which resolves each card id
	// against noteboard concurrently and stores nil for a 404 while keeping the
	// slice index-aligned with the ids. So item==null means precisely "noteboard
	// has no live row for this card id" — a deleted todo, since noteboard's
	// delete is a soft one that drops the row out of every read path. It does
	// NOT mean "held": noteboard's GetItem filters on deleted_at only, so a held
	// item still resolves and is returned here rather than being skipped.
	client, _ := stubKanban(t, http.StatusOK, `[
		{"card_id":"todo-deleted","item":null},
		{"card_id":"todo-alive","item":{"id":"todo-alive"}}
	]`)

	got, err := client.LinkedTodoForSession(context.Background(), "br_1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "todo-alive" {
		t.Errorf("todo = %q, want todo-alive", got)
	}
}

// A row whose card_id is absent or blank is skipped, and the next usable card
// is returned.
//
// kanban-store cannot emit this: the link route takes the card id from the URL
// path, refuses an empty rest, and Go's mux collapses "/api/cards//links"
// before the handler sees it — measured, and zero of the 6,590 live session
// links have a blank card_id. So this pins the client's tolerance of a
// malformed body rather than a behaviour of the store.
//
// The good row after the blank one is what makes the test discriminating. With
// only the blank row present, the client returns "" — and so does a client
// with the skip deleted, so that version asserts nothing. Scored: the
// blank-row-only case left the skip untested.
func TestLinkedTodoForSessionSkipsRowsWithNoCardID(t *testing.T) {
	client, _ := stubKanban(t, http.StatusOK, `[
		{"item":{"id":"no-card-id"}},
		{"card_id":"","item":{"id":"blank-card-id"}},
		{"card_id":"todo-alive","item":{"id":"todo-alive"}}
	]`)

	got, err := client.LinkedTodoForSession(context.Background(), "br_1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "todo-alive" {
		t.Errorf("todo = %q, want todo-alive", got)
	}
}

func TestLinkedTodoForSessionWithNoLinks(t *testing.T) {
	for name, body := range map[string]string{
		// What the running kanban-store actually returns for a session with no
		// links, and for a session ref it has never heard of: 200 with an empty
		// array. Both were probed on :8305 rather than assumed — the route
		// builds its response with make([]map[string]any, len(ids)), which
		// marshals to [] and never to null.
		"empty array": `[]`,
		// Not a shape kanban-store emits; kept because a JSON null decodes into
		// a nil slice and the loop must not treat that as an error.
		"json null":    `null`,
		"all orphaned": `[{"card_id":"gone","item":null}]`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := stubKanban(t, http.StatusOK, body)
			got, err := client.LinkedTodoForSession(context.Background(), "br_1")
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if got != "" {
				t.Errorf("todo = %q, want empty", got)
			}
		})
	}
}

// A store that answers with an error or with something that is not the
// documented shape must produce an error, never a confident empty string:
// the caller logs an error but treats "" as "this session has no todo".
func TestLinkedTodoForSessionErrors(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		client, _ := stubKanban(t, http.StatusInternalServerError, `{"error":"boom"}`)
		_, err := client.LinkedTodoForSession(context.Background(), "br_1")
		if err == nil {
			t.Fatal("expected an error for a 500")
		}
		// The status has to reach the message, and asserting merely that an
		// error came back does not check that. kanban-store's error bodies are
		// JSON objects, so with the status check deleted the read still fails
		// one step later in the decoder — reporting a refused read as a
		// malformed one, and losing the 500 that says which service is down.
		// Scored: deleting the status check left the old assertion green.
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want it to name the 500 status", err)
		}
	})
	t.Run("502 when kanban-store cannot reach noteboard", func(t *testing.T) {
		// kanban-store answers 502, not 500, when its own noteboard lookup
		// fails — a distinct code for "my upstream is down" that the message
		// must carry through for the same reason.
		client, _ := stubKanban(t, http.StatusBadGateway, `{"error":"noteboard: connection refused"}`)
		_, err := client.LinkedTodoForSession(context.Background(), "br_1")
		if err == nil {
			t.Fatal("expected an error for a 502")
		}
		if !strings.Contains(err.Error(), "502") {
			t.Errorf("error = %q, want it to name the 502 status", err)
		}
	})
	t.Run("unparseable body", func(t *testing.T) {
		client, _ := stubKanban(t, http.StatusOK, `{"not":"an array"}`)
		if _, err := client.LinkedTodoForSession(context.Background(), "br_1"); err == nil {
			t.Fatal("expected an error for a body that is not the documented shape")
		}
	})
	t.Run("empty session id", func(t *testing.T) {
		client, _ := stubKanban(t, http.StatusOK, `[]`)
		if _, err := client.LinkedTodoForSession(context.Background(), ""); err == nil {
			t.Fatal("expected an error for an empty session id")
		}
	})
}

// The cry-wolf control for the error assertions above: the ordinary quiet case
// — a real session that simply has no links yet — must stay silent. Every
// repair in this family fails by making the error surface fire on the
// legitimate case, and "" with a nil error is what the caller reads as "this
// session has no todo".
func TestLinkedTodoForSessionIsQuietWhenThereIsNothingToFind(t *testing.T) {
	client, _ := stubKanban(t, http.StatusOK, `[]`)
	got, err := client.LinkedTodoForSession(context.Background(), "br_1784416900461396845")
	if err != nil {
		t.Fatalf("a session with no links is not an error: %v", err)
	}
	if got != "" {
		t.Errorf("todo = %q, want empty", got)
	}
}

// The client percent-encodes the session id into the path segment. That stops
// the id from forging extra path segments in the request line — but it does
// NOT make such an id work, because kanban-store routes on the decoded path.
//
// Measured against the running kanban-store on :8305: the request line this
// client sends for "br_1/../br_2" is
// /api/entities/session/br_1%2F..%2Fbr_2/cards, and the answer is
// 404 {"error":"not found"} — not a redirect, and not br_2's cards. The
// server decodes %2F back to a slash in URL.Path, so kanban-store's split
// sees ["session","br_1","..","br_2","cards"], reads ".." as the sub-resource,
// and falls through to its default 404.
//
// So the escaping's real job is containment, not compatibility: an id carrying
// a slash fails loudly with a status in the message, rather than silently
// resolving to a DIFFERENT session's cards. Asserting err == nil here — which
// is what the old test did, against a fixture that served every path alike —
// pinned behaviour kanban-store does not have.
//
// Every session id in use today (br_…, autoworker-anthropic-…, herald-…,
// tcl-…) is already free of slashes: measured over all 6,590 session links in
// kanban-store's live card_links, zero contain a slash, a percent or a space.
// This guards the boundary against a future id shape.
func TestLinkedTodoForSessionEscapesTheSessionIDIntoOneSegment(t *testing.T) {
	client, path := stubKanban(t, http.StatusOK, `[{"card_id":"br_2-todo","item":{"id":"br_2-todo"}}]`)

	got, err := client.LinkedTodoForSession(context.Background(), "br_1/../br_2")

	// The request line must carry the id as a single escaped segment.
	if want := "/api/entities/session/br_1%2F..%2Fbr_2/cards"; *path != want {
		t.Errorf("path = %q, want %q", *path, want)
	}
	// And kanban-store refuses it rather than serving br_2.
	if err == nil {
		t.Fatalf("expected an error; kanban-store 404s this path, got todo %q", got)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to name the 404 status", err)
	}
	if got != "" {
		t.Errorf("todo = %q, want empty — never another session's card", got)
	}
}
