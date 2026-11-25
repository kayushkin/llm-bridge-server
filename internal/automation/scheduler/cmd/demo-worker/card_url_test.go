package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCardURLsKeepEachIDOnePathSegment pins that a card or board id holding
// "/" or "?" reaches kanban-store and noteboard as one escaped segment.
// Unescaped, "a/b" would hit a different route and "a?b" would cut the id
// short at the query separator.
func TestCardURLsKeepEachIDOnePathSegment(t *testing.T) {
	const cardID = "card/with?query"
	const escapedCardID = "card%2Fwith%3Fquery"

	var gotEscapedPath, gotRawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		gotRawQuery = r.URL.RawQuery
		switch {
		case r.Method == http.MethodGet && gotEscapedPath == "/api/cards/"+escapedCardID+"/events":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	cases := []struct {
		name      string
		call      func() error
		wantPath  string
		wantQuery string
	}{
		{"patchItem", func() error { return patchItem(server.URL, cardID, map[string]any{"title": "x"}) },
			"/api/cards/" + escapedCardID, ""},
		{"addNote", func() error { return addNote(server.URL, cardID, "board-1", "tester", "body") },
			"/api/cards/" + escapedCardID + "/notes", ""},
		{"linkAs", func() error { return linkAs(server.URL, cardID, "session", "s-1", "label", "a&b") },
			"/api/cards/" + escapedCardID + "/links", "actor=a%26b"},
		{"getItem", func() error { _, err := getItem(server.URL, cardID); return err },
			"/api/items/" + escapedCardID, ""},
		{"unhold", func() error { return unhold(server.URL, cardID) },
			"/api/cards/" + escapedCardID + "/unhold", ""},
		{"listEvents", func() error { _, err := listEvents(server.URL, cardID); return err },
			"/api/cards/" + escapedCardID + "/events", ""},
		{"attachCard", func() error { return attachCard(server.URL, "board/b?q", cardID, "column-1") },
			"/api/boards/board%2Fb%3Fq/cards/" + escapedCardID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEscapedPath, gotRawQuery = "", ""
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotEscapedPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotEscapedPath, tc.wantPath)
			}
			if gotRawQuery != tc.wantQuery {
				t.Errorf("query = %q, want %q", gotRawQuery, tc.wantQuery)
			}
		})
	}
}
