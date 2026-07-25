package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// TestHandleListSessions_LimitOffset exercises the additive limit/offset
// bounding on GET /sessions at the handler level, complementing the
// store-level TestListSessionsPaged. It pins three properties the fix
// promised: the default response is unchanged (no bound), limit/offset page
// through the rows, and a malformed bound fails loudly with 400 rather than
// being silently ignored (the historical trap where ?limit= was accepted but
// did nothing).
func TestHandleListSessions_LimitOffset(t *testing.T) {
	srv, st := testServer(t)

	// Five sessions, created oldest→newest so DESC (created_at) order is e..a.
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		if err := st.CreateSession(&store.Session{
			SessionID:   "br_" + name,
			DisplayName: name,
			Harness:     "claude_code",
			State:       "idle",
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		time.Sleep(time.Duration(i) * time.Millisecond)
	}

	list := func(t *testing.T, query string) ([]store.Session, int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/sessions"+query, nil)
		w := httptest.NewRecorder()
		srv.handleListSessions(w, req)
		if w.Code != http.StatusOK {
			return nil, w.Code
		}
		var got []store.Session
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode body %q: %v", w.Body.String(), err)
		}
		return got, w.Code
	}

	// No bound → historical behavior: every row.
	if got, code := list(t, ""); code != http.StatusOK || len(got) != 5 {
		t.Fatalf("bare list: code=%d count=%d, want 200/5", code, len(got))
	}

	// limit=2 → newest two, DESC.
	if got, code := list(t, "?limit=2"); code != http.StatusOK || len(got) != 2 ||
		got[0].DisplayName != "e" || got[1].DisplayName != "d" {
		t.Fatalf("?limit=2 = %+v (code %d), want [e d]", got, code)
	}

	// limit=2&offset=2 → next page.
	if got, code := list(t, "?limit=2&offset=2"); code != http.StatusOK || len(got) != 2 ||
		got[0].DisplayName != "c" || got[1].DisplayName != "b" {
		t.Fatalf("?limit=2&offset=2 = %+v (code %d), want [c b]", got, code)
	}

	// Malformed / negative bounds fail loudly (400), never silently ignored.
	for _, q := range []string{"?limit=abc", "?limit=-1", "?offset=-5", "?offset=nope"} {
		if _, code := list(t, q); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400", q, code)
		}
	}
}
