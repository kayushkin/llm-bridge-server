package kanbanclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubKanban serves one canned body from the session-cards route and records
// the path it was asked for.
func stubKanban(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI is the raw request line. r.URL.Path would show the
		// server's percent-decoding of it, which is not what this asserts.
		gotPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
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

func TestLinkedTodoForSessionWithNoLinks(t *testing.T) {
	for name, body := range map[string]string{
		"empty array":  `[]`,
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
		if _, err := client.LinkedTodoForSession(context.Background(), "br_1"); err == nil {
			t.Fatal("expected an error for a 500")
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

// The session id goes into a path segment, so the client percent-encodes it
// rather than pasting it in. Every id in use here (br_…,
// autoworker-anthropic-…, herald-…, tcl-…) is already safe, so this guards
// the client against a future id shape rather than a live bug.
func TestLinkedTodoForSessionEscapesTheSessionID(t *testing.T) {
	// The space case is not decoration. PathEscape and QueryEscape agree on
	// "/" — both write %2F — so a slash cannot tell them apart, and swapping
	// one for the other survives an id that only contains slashes. They
	// disagree on a space: QueryEscape writes "+", which inside a PATH segment
	// is a literal plus, not a space. That addresses a different entity ref,
	// and kanban-store answers an unknown ref with 200 and an empty list, so
	// the mistake would be silent.
	for _, tc := range []struct {
		name      string
		sessionID string
		want      string
	}{
		{"path traversal", "br_1/../br_2", "/api/entities/session/br_1%2F..%2Fbr_2/cards"},
		{"space", "br 1", "/api/entities/session/br%201/cards"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, path := stubKanban(t, http.StatusOK, `[]`)
			if _, err := client.LinkedTodoForSession(context.Background(), tc.sessionID); err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if *path != tc.want {
				t.Errorf("path = %q, want %q", *path, tc.want)
			}
		})
	}
}
