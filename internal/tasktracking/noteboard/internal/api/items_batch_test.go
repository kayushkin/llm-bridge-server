package api_test

import (
	"bytes"
	"encoding/json"
	"github.com/kayushkin/noteboard/model"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestBatchPreservesOrderFullBodiesHeldAndMissingSlots(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	body := "  完整 body\nwith whitespace\t"
	live := createItem(t, h, map[string]any{"title": "ÉCOLE 100%_", "body": body, "hold": true})
	deleted := createItem(t, h, map[string]any{"title": "deleted"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/items/"+deleted, nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	payload, _ := json.Marshal(model.ItemsBatchRequest{IDs: []string{live, "missing", deleted, live}})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/items/batch", bytes.NewReader(payload)))
	if w.Code != 200 {
		t.Fatalf("batch: %d %s", w.Code, w.Body.String())
	}
	var result model.ItemsBatchResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 4 || result.Items[1] != nil || result.Items[2] != nil || result.Items[0].Body != body || !reflect.DeepEqual(result.Items[0], result.Items[3]) {
		t.Fatalf("bad batch: %+v", result)
	}
	for _, raw := range []string{`{}`, `{"ids":[""]}`, `{"ids":[],"status":"done"}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/items/batch", bytes.NewBufferString(raw)))
		if w.Code != 400 {
			t.Errorf("bad batch accepted: %s", raw)
		}
	}
	// Search is literal and Unicode-aware; '%' and '_' never act as SQL wildcards.
	for _, term := range []string{"école", "100%_", "完整", live} {
		result := queryItems(t, h, map[string]any{"ids": []string{deleted, live}, "q": term, "limit": 1, "include_items": true}, 200)
		if result.Total != 1 || len(result.Items) != 1 || result.Items[0].Body != body {
			t.Errorf("q %q: %+v", term, result)
		}
	}
	if result := queryItems(t, h, map[string]any{"ids": []string{live}, "q": "not in body", "limit": 1}, 200); result.Total != 0 {
		t.Fatal("query ignored")
	}
}

func TestQueryCreationBoundsAndAscendingUpdatedOrder(t *testing.T) {
	a, cleanup := setup(t)
	defer cleanup()
	h := a.Handler()
	first := createItem(t, h, map[string]any{"title": "first"})
	second := createItem(t, h, map[string]any{"title": "second"})
	ids := []string{second, first}
	result := queryItems(t, h, map[string]any{"ids": ids, "sort": "updated_asc", "limit": 1, "include_items": true}, 200)
	if len(result.IDs) != 1 || result.IDs[0] != first || result.Total != 2 {
		t.Fatalf("ascending page: %+v", result)
	}
	result = queryItems(t, h, map[string]any{"ids": ids, "created_from": "1900-01-01T00:00:00Z", "created_to": "2100-01-01T00:00:00Z", "limit": 1, "offset": 1}, 200)
	if result.Total != 2 || len(result.IDs) != 1 || result.IDs[0] != first {
		t.Fatalf("date page: %+v", result)
	}
	result = queryItems(t, h, map[string]any{"ids": ids, "created_to": "1900-01-01T00:00:00Z"}, 200)
	if result.Total != 0 {
		t.Fatal("date filter ignored")
	}
	queryItems(t, h, map[string]any{"ids": ids, "created_from": "not-a-date"}, 400)
}
