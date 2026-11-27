package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/bridgestores"
)

// TestCardURLsKeepEachIDOnePathSegment pins that a card or board id holding
// "/" or "?" reaches the bridge's issues as one escaped segment.
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
		case r.Method == http.MethodGet && gotEscapedPath == "/issues/cards/"+escapedCardID+"/events":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	issuesURL := bridgestores.IssuesURL(server.URL)

	cases := []struct {
		name      string
		call      func() error
		wantPath  string
		wantQuery string
	}{
		{"patchItem", func() error { return patchItem(issuesURL, cardID, map[string]any{"title": "x"}) },
			"/issues/cards/" + escapedCardID, ""},
		{"addNote", func() error { return addNote(issuesURL, cardID, "board-1", "tester", "body") },
			"/issues/cards/" + escapedCardID + "/notes", ""},
		{"linkAs", func() error { return linkAs(issuesURL, cardID, "session", "s-1", "label", "a&b") },
			"/issues/cards/" + escapedCardID + "/links", "actor=a%26b"},
		{"getItem", func() error { _, err := getItem(issuesURL, cardID); return err },
			"/issues/items/" + escapedCardID, ""},
		{"unhold", func() error { return unhold(issuesURL, cardID) },
			"/issues/cards/" + escapedCardID + "/unhold", ""},
		{"listEvents", func() error { _, err := listEvents(issuesURL, cardID); return err },
			"/issues/cards/" + escapedCardID + "/events", ""},
		{"attachCard", func() error { return attachCard(issuesURL, "board/b?q", cardID, "column-1") },
			"/issues/boards/board%2Fb%3Fq/cards/" + escapedCardID, ""},
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
