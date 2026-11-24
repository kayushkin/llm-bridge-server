package api_test

import (
	"net/http/httptest"
	"testing"
)

func TestAssignmentETagRejectsStaleAutomaticAssignment(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	_, _, id := assignmentTestCard(t, h)
	path := "/api/cards/" + id + "/assignments"
	snapshot := do(t, h, "GET", path, nil)
	version := snapshot.Header().Get("ETag")
	if version == "" {
		t.Fatal("missing assignment ETag")
	}
	first := do(t, h, "PUT", path+"/"+activePrincipal, nil)
	if first.Code != 201 {
		t.Fatal(first.Body.String())
	}
	req := httptest.NewRequest("PUT", path+"/"+otherActivePrincipal, nil)
	req.Header.Set("If-Match", version)
	req.Header.Set("X-Kanban-Store-Service-Token", testServiceToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 412 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
