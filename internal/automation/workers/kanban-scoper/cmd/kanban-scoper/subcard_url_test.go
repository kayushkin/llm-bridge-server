package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// TestCreateSubcardKeepsTheBoardIDOnePathSegment pins that a board id holding
// "/" or "?" reaches kanban-store as one escaped segment. Unescaped, "a/b"
// would POST to a different route and "a?b" would cut the id short at the
// query separator.
func TestCreateSubcardKeepsTheBoardIDOnePathSegment(t *testing.T) {
	var gotEscapedPath, gotRawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && gotEscapedPath == "" {
			gotEscapedPath = r.URL.EscapedPath()
			gotRawQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"item":{"id":"child-1"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"link-1"}`))
	}))
	defer server.Close()

	kc := &kanbanclient.Client{BaseURL: server.URL, HTTP: server.Client(), BoardID: "board/with?query"}
	if _, err := createSubcard(kc, "column-1", "title", "body", nil, "parent-1"); err != nil {
		t.Fatalf("createSubcard: %v", err)
	}
	if want := "/api/boards/board%2Fwith%3Fquery/cards"; gotEscapedPath != want {
		t.Errorf("POST path = %q, want %q", gotEscapedPath, want)
	}
	if gotRawQuery != "" {
		t.Errorf("POST carried query %q; the board id leaked past the path", gotRawQuery)
	}
}
