package possessionstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheRoutesRegisterAndAnswer(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	(&Server{Store: store, Reviewer: &Reviewer{Store: store, Photos: fakePhotos{}}}).RegisterHandlers(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	created := call("POST", "/possessions", `{"actor":"principal_000001","name":"laptop","computer":{"form_factor":"laptop","harness_machine_id":"m_test_machine"},
		"photos":[{"photo_id":"photo_000001","box_left":0.25,"box_top":0.25,"box_right":0.75,"box_bottom":0.75}]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /possessions = %d %s", created.Code, created.Body)
	}
	for path, want := range map[string]int{
		"/harness-machines/m_test_machine/computer": 200,
		"/computers/computer_000001":                200,
		"/computers":                                200,
		"/garments":                                 200,
		"/possessions/possession_000001/image":      200,
		"/computers/computer_000001/image":          200,
		"/possessions/possession_999999":            404,
		"/possessions?kind=lamp":                    400,
		"/vocabulary":                               200,
		"/kinds":                                    200,
		"/health":                                   200,
		"/owner-questions":                          200,
	} {
		if got := call("GET", path, ""); got.Code != want {
			t.Errorf("GET %s = %d %s, want %d", path, got.Code, got.Body, want)
		}
	}
	image := call("GET", "/possessions/possession_000001/image", "")
	if image.Header().Get("Content-Type") != "image/jpeg" || image.Body.Len() == 0 {
		t.Errorf("the image is %q, %d bytes", image.Header().Get("Content-Type"), image.Body.Len())
	}
	if got := call("PATCH", "/possessions/possession_000001", `{"actor":"principal_000001","nickname":"x"}`); got.Code != 400 {
		t.Errorf("PATCH with an unknown field = %d, want 400", got.Code)
	}
	var health struct {
		Counts map[string]int `json:"counts"`
	}
	_ = json.Unmarshal(call("GET", "/health", "").Body.Bytes(), &health)
	if health.Counts["possessions"] != 1 {
		t.Errorf("health counts = %v", health.Counts)
	}
}
