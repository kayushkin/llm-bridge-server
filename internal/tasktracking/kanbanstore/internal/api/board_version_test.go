package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/api"
)

func TestBoardETagConditionalPatch(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	id := mkBoard(t, h, "Versioned board")
	get := do(t, h, "GET", "/api/boards/"+id, nil)
	etag := get.Header().Get("ETag")
	if get.Code != 200 || etag == "" {
		t.Fatalf("GET: %d, ETag %q", get.Code, etag)
	}
	patch := func(condition, name string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("PATCH", "/api/boards/"+id, strings.NewReader(`{"name":"`+name+`"}`))
		request.Header.Set(api.ServiceTokenHeader, testServiceToken)
		if condition != "" {
			request.Header.Set("If-Match", condition)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		return response
	}
	saved := patch(etag, "First writer")
	if saved.Code != http.StatusOK || saved.Header().Get("ETag") == etag {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	stale := patch(etag, "Stale writer")
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale: %d %s", stale.Code, stale.Body.String())
	}
	if got := getBoard(t, h, id); got.Name != "First writer" {
		t.Fatalf("stale save changed board: %+v", got)
	}
	for _, condition := range []string{"unquoted", `W/` + etag, `"invalid"`, etag + "," + etag} {
		if response := patch(condition, "Invalid"); response.Code != http.StatusBadRequest {
			t.Fatalf("condition %q: %d", condition, response.Code)
		}
	}
	if response := patch("", "Legacy writer"); response.Code != http.StatusOK {
		t.Fatalf("legacy: %d %s", response.Code, response.Body.String())
	}
	if response := patch("*", "Wildcard writer"); response.Code != http.StatusOK {
		t.Fatalf("wildcard: %d %s", response.Code, response.Body.String())
	}
}
