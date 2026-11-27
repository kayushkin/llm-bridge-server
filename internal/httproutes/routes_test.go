package httproutes

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRecordingMuxRecordsAndServesEveryPattern(t *testing.T) {
	mux := NewRecordingMux()
	mux.HandleFunc("GET /a", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mux.Handle("POST /b/{id}", http.NotFoundHandler())
	if got, want := mux.Patterns(), []string{"GET /a", "POST /b/{id}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Patterns() = %v, want %v", got, want)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/a", nil))
	if response.Code != http.StatusTeapot {
		t.Fatalf("GET /a = %d, want the registered handler's 418", response.Code)
	}
}
