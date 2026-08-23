package kanbanclient

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// probeIDs are the seven ids the url-escape sweep drives every repaired client
// with. Six are path-significant; "card-123" is the well-formed control and
// must be unaffected by the repair.
var probeIDs = []string{
	"card-123",        // control: well-formed, must not change
	"a/b",             // adds a path segment
	"sess?agent=root", // opens a query string
	"sess#frag",       // net/http drops the rest of the URL
	"../boards",       // climbs out of the collection
	"sess%2Fb",        // arrives DECODED at the server, so a slash again
	"a b",             // a space; Go escapes this one on its own
}

// recorder captures r.RequestURI for one request.
//
// It must be RequestURI and never r.URL.Path: Go's server decodes %2F back to
// a slash before it fills URL.Path, so a URL.Path assertion reads the same
// whether or not the client escaped anything and cannot hold this property.
func recorder(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cards") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"c1"}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"x1"}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// assertSegments checks the property directly rather than re-deriving the
// expected string with url.PathEscape — a fixture built out of the function
// under test narrows in step with the defect. The claim is: the request path
// is exactly these segments, and each one decodes back to what the caller
// asked for.
func assertSegments(t *testing.T, caller, id, gotURI string, want []string) {
	t.Helper()
	if i := strings.IndexAny(gotURI, "?#"); i >= 0 {
		t.Errorf("%s id=%q: request carries a query or fragment it was never given: %q",
			caller, id, gotURI)
		return
	}
	parts := strings.Split(strings.TrimPrefix(gotURI, "/"), "/")
	if len(parts) != len(want) {
		t.Errorf("%s id=%q: %d path segments, want %d: %q",
			caller, id, len(parts), len(want), gotURI)
		return
	}
	for i := range want {
		dec, err := url.PathUnescape(parts[i])
		if err != nil {
			t.Errorf("%s id=%q: segment %d %q does not decode: %v", caller, id, i, parts[i], err)
			continue
		}
		if dec != want[i] {
			t.Errorf("%s id=%q: segment %d is %q (decoded %q), want %q — %s",
				caller, id, i, parts[i], dec, want[i], gotURI)
		}
	}
}

// TestAKanbanIDOrTagStaysOnePathSegment drives every caller that puts a value
// into a kanban-store URL path. Every id here is chosen by another process:
// kanban-store minted the board, card and link ids, llm-bridge-server minted
// the session ids used as entity refs, and the tags are read back out of
// kanban-store before being sent again.
func TestAKanbanIDOrTagStaysOnePathSegment(t *testing.T) {
	const ok = "well-formed"

	callers := []struct {
		name string
		// call runs the caller with probe substituted for its one value under
		// test, and returns the path segments the request must have.
		call func(c *Client, probe string) []string
	}{
		{"ListColumns/boardID", func(c *Client, p string) []string {
			c.ListColumns(p)
			return []string{"api", "boards", p, "columns"}
		}},
		{"GetBoardView/boardID", func(c *Client, p string) []string {
			c.GetBoardView(p)
			return []string{"api", "boards", p, "cards"}
		}},
		{"CreateCard/BoardID", func(c *Client, p string) []string {
			c.BoardID = p
			c.ColumnsByName = map[string]string{"Todo": "col1"}
			c.CreateCard(CreateCardArgs{ColumnName: "Todo", Title: "t"})
			return []string{"api", "boards", p, "cards"}
		}},
		{"AddCardLink/cardID", func(c *Client, p string) []string {
			c.AddCardLink(p, "session", ok, "")
			return []string{"api", "cards", p, "links"}
		}},
		{"ListCardLinks/cardID", func(c *Client, p string) []string {
			c.ListCardLinks(p)
			return []string{"api", "cards", p, "links"}
		}},
		{"MoveCard/cardID", func(c *Client, p string) []string {
			c.MoveCard(p, "b1", "col1", 1)
			return []string{"api", "cards", p, "move"}
		}},
		{"PatchCardBody/cardID", func(c *Client, p string) []string {
			c.PatchCardBody(p, "body")
			return []string{"api", "cards", p}
		}},
		{"ListEntityTags/entityRef", func(c *Client, p string) []string {
			c.ListEntityTags("session", p)
			return []string{"api", "entities", "session", p, "tags"}
		}},
		{"ListEntityTags/entityType", func(c *Client, p string) []string {
			c.ListEntityTags(p, ok)
			return []string{"api", "entities", p, ok, "tags"}
		}},
		{"AddEntityTag/entityRef", func(c *Client, p string) []string {
			c.AddEntityTag("session", p, "tag")
			return []string{"api", "entities", "session", p, "tags"}
		}},
		{"DeleteEntityTag/entityRef", func(c *Client, p string) []string {
			c.DeleteEntityTag("session", p, "tag")
			return []string{"api", "entities", "session", p, "tags", "tag"}
		}},
		{"DeleteEntityTag/tag", func(c *Client, p string) []string {
			c.DeleteEntityTag("session", ok, p)
			return []string{"api", "entities", "session", ok, "tags", p}
		}},
		{"ListCardsForEntity/entityRef", func(c *Client, p string) []string {
			c.ListCardsForEntity("session", p)
			return []string{"api", "entities", "session", p, "cards"}
		}},
		{"GetBoard/id", func(c *Client, p string) []string {
			c.GetBoard(p)
			return []string{"api", "boards", p}
		}},
		{"GetCardEffectiveDefaults/boardID", func(c *Client, p string) []string {
			c.GetCardEffectiveDefaults(p, ok)
			return []string{"api", "boards", p, "cards", ok, "effective-defaults"}
		}},
		{"GetCardEffectiveDefaults/cardID", func(c *Client, p string) []string {
			c.GetCardEffectiveDefaults(ok, p)
			return []string{"api", "boards", ok, "cards", p, "effective-defaults"}
		}},
		{"PutTicket/cardID", func(c *Client, p string) []string {
			c.PutTicket(p, "principal_000001", "chat", "session", ok)
			return []string{"api", "cards", p, "ticket"}
		}},
		{"PatchCard/cardID", func(c *Client, p string) []string {
			c.PatchCard(p, map[string]any{"title": "t"})
			return []string{"api", "cards", p}
		}},
		{"HoldCard/cardID", func(c *Client, p string) []string {
			c.HoldCard(p, "reason")
			return []string{"api", "cards", p, "hold"}
		}},
		{"DeleteCardLink/linkID", func(c *Client, p string) []string {
			c.DeleteCardLink(p)
			return []string{"api", "links", p}
		}},
		{"DetachCardFromBoard/boardID", func(c *Client, p string) []string {
			c.DetachCardFromBoard(p, ok)
			return []string{"api", "boards", p, "cards", ok}
		}},
		{"DetachCardFromBoard/cardID", func(c *Client, p string) []string {
			c.DetachCardFromBoard(ok, p)
			return []string{"api", "boards", ok, "cards", p}
		}},
	}

	for _, tc := range callers {
		for _, probe := range probeIDs {
			t.Run(tc.name+"/"+probe, func(t *testing.T) {
				srv, got := recorder(t)
				c := New(srv.URL, "")
				want := tc.call(c, probe)
				assertSegments(t, tc.name, probe, *got, want)
			})
		}
	}
}
