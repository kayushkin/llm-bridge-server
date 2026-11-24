package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kayushkin/noteboard/model"
)

func patchItem(t *testing.T, h http.Handler, id, ifMatch string, patch map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PATCH", "/api/items/"+id, bytes.NewReader(body))
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func createNote(t *testing.T, h http.Handler, title string) model.Item {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"type": "note", "title": title})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/items", bytes.NewReader(body)))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var item model.Item
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

// Two people open the same item. The first saves. The second's save was made
// against a version that is gone, and applying it would overwrite the first
// one's change without either of them seeing it.
func TestAPatchMadeAgainstAnOlderVersionIsRefusedAndWritesNothing(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	opened := createNote(t, h, "as both of them opened it")

	// The version is the item's updated_at exactly as its JSON carries it, which
	// is also what GET sends as the ETag.
	raw, _ := json.Marshal(opened.UpdatedAt)
	versionBothHold := string(raw) // already quoted

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/api/items/"+opened.ID, nil))
	if got := get.Header().Get("ETag"); got != versionBothHold {
		t.Fatalf("GET ETag = %s, want the item's updated_at %s", got, versionBothHold)
	}

	first := patchItem(t, h, opened.ID, versionBothHold, map[string]any{"title": "the first save"})
	if first.Code != 200 {
		t.Fatalf("first save: %d %s", first.Code, first.Body.String())
	}
	if first.Header().Get("ETag") == versionBothHold {
		t.Fatalf("the save answered the old version as its ETag")
	}

	second := patchItem(t, h, opened.ID, versionBothHold, map[string]any{"title": "the second save"})
	if second.Code != http.StatusPreconditionFailed {
		t.Fatalf("second save: %d %s, want 412", second.Code, second.Body.String())
	}
	var refusal struct {
		Error   string      `json:"error"`
		Current *model.Item `json:"current"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Current == nil || refusal.Current.Title != "the first save" {
		t.Fatalf("412 body = %s, want the item as the first save left it", second.Body.String())
	}

	// Nothing was written: not the title, and no revision for a refused update.
	revisions := httptest.NewRecorder()
	h.ServeHTTP(revisions, httptest.NewRequest("GET", "/api/items/"+opened.ID+"/revisions", nil))
	var listed []json.RawMessage
	if err := json.Unmarshal(revisions.Body.Bytes(), &listed); err != nil {
		t.Fatalf("revisions: %s", revisions.Body.String())
	}
	if len(listed) != 1 {
		t.Errorf("%d revisions, want 1: the refused update must not have snapshotted", len(listed))
	}

	// Retrying against the version the 412 handed back succeeds.
	raw, _ = json.Marshal(refusal.Current.UpdatedAt)
	if retry := patchItem(t, h, opened.ID, string(raw), map[string]any{"title": "the second save"}); retry.Code != 200 {
		t.Fatalf("retry against the current version: %d %s", retry.Code, retry.Body.String())
	}
}

// Every caller that existed before the header did sends none, and must see no
// change. "*" asks for the same.
func TestAPatchWithoutAPreconditionIsAppliedWhateverTheVersion(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	item := createNote(t, h, "before")
	for _, ifMatch := range []string{"", "*"} {
		if w := patchItem(t, h, item.ID, ifMatch, map[string]any{"title": "after " + ifMatch}); w.Code != 200 {
			t.Fatalf("If-Match %q: %d %s", ifMatch, w.Code, w.Body.String())
		}
	}
}

// A precondition that cannot be read must not be dropped: its sender believes
// the update is conditional.
func TestAnUnreadablePreconditionIsRefusedNotIgnored(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	item := createNote(t, h, "untouched")
	if w := patchItem(t, h, item.ID, `"version-7"`, map[string]any{"title": "written anyway"}); w.Code != 400 {
		t.Fatalf("unreadable If-Match: %d %s, want 400", w.Code, w.Body.String())
	}
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/api/items/"+item.ID, nil))
	var after model.Item
	json.Unmarshal(get.Body.Bytes(), &after)
	if after.Title != "untouched" {
		t.Fatalf("title = %q: the update was applied", after.Title)
	}
}

// Many saves against one version, at once: exactly one may win. Without the
// store's mutex two of them can both pass the check before either writes.
func TestOnlyOneOfManyConcurrentSavesAgainstOneVersionWins(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	item := createNote(t, h, "contested")
	raw, _ := json.Marshal(item.UpdatedAt)

	const savers = 16
	codes := make([]int, savers)
	var wg sync.WaitGroup
	for i := 0; i < savers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = patchItem(t, h, item.ID, string(raw), map[string]any{"body": "saver"}).Code
		}(i)
	}
	wg.Wait()
	won, refused := 0, 0
	for _, code := range codes {
		switch code {
		case 200:
			won++
		case http.StatusPreconditionFailed:
			refused++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if won != 1 || refused != savers-1 {
		t.Fatalf("%d won and %d were refused, want 1 and %d", won, refused, savers-1)
	}
}
