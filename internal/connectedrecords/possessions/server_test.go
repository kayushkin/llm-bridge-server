package possessions

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
	registerEveryRoute(mux, &Reviewer{Store: store, Photos: fakePhotos{}})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		asTestOwner(mux).ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	created := call("POST", "/possessions", `{"actor":"principal_000001","name":"laptop","computer":{"form_factor":"laptop","harness_machine_id":"m_test_machine"},
		"photos":[{"photo_id":"photo_000001","box_left":0.25,"box_top":0.25,"box_right":0.75,"box_bottom":0.75}]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /possessions = %d %s", created.Code, created.Body)
	}
	for path, want := range map[string]int{
		"/harness-machines/m_test_machine/computer":  200,
		"/computers/computer_000001":                 200,
		"/computers":                                 200,
		"/garments":                                  200,
		"/possessions/possession_000001/image":       200,
		"/computers/computer_000001/image":           200,
		"/possessions/possession_999999":             404,
		"/possessions?kind=lamp":                     400,
		"/vocabulary":                                200,
		"/kinds":                                     200,
		"/health":                                    200,
		"/owner-questions":                           200,
		"/possession-merges":                         200,
		"/possession-merges/possession_merge_000001": 404,
		"/outfit-merges":                             200,
		"/wearable-groups":                           200,
		"/outfit-looks":                              200,
		"/outfit-looks/outfit_look_000001":           404,
		"/outfit-slots/outfit_slot_000001":           404,
		"/outfit-rebuilds/outfit_rebuild_000001":     404,
		"/outfit-merges/outfit_merge_000001":         404,
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
	if got := call("PUT", "/wearable-groups/jewelry:hat", `{"closed":true,"actor":"principal_000001"}`); got.Code != 400 {
		t.Errorf("closing a group that cannot exist = %d, want 400", got.Code)
	}
	if got := call("PUT", "/wearable-groups/watch", `{"closed":true,"actor":"principal_000001"}`); got.Code != 200 || !strings.Contains(got.Body.String(), `"closed":true`) {
		t.Errorf("closing watch = %d %s", got.Code, got.Body)
	}
	for _, path := range []string{"/wearables/check-links", "/wearables/check-duplicates", "/outfits/consolidate", "/outfit-pieces/rematch", "/outfit-looks/read", "/outfit-slots/identify", "/outfits/rebuild-from-looks"} {
		if got := call("POST", path, `{}`); got.Code != 400 {
			t.Errorf("POST %s with no actor = %d %s, want 400", path, got.Code, got.Body)
		}
	}
	if got := call("POST", "/possession-merges/possession_merge_000001/undo", `{"actor":"principal_000001"}`); got.Code != 404 {
		t.Errorf("undo of no merge = %d, want 404", got.Code)
	}
	if got := call("POST", "/outfit-merges/outfit_merge_000001/undo", `{"actor":"principal_000001"}`); got.Code != 404 {
		t.Errorf("undo of no outfit merge = %d, want 404", got.Code)
	}
	var health struct {
		Counts map[string]int `json:"counts"`
	}
	_ = json.Unmarshal(call("GET", "/health", "").Body.Bytes(), &health)
	if health.Counts["possessions"] != 1 {
		t.Errorf("health counts = %v", health.Counts)
	}
}

// registerEveryRoute registers the routes of the records, the review and
// the outfits on mux, as the bridge mounts them.
func registerEveryRoute(mux *http.ServeMux, reviewer *Reviewer) {
	(&Server{Store: reviewer.Store, Photos: reviewer.Photos}).RegisterHandlers(mux)
	(&ReviewServer{Reviewer: reviewer}).RegisterHandlers(mux)
	(&OutfitServer{Reviewer: reviewer}).RegisterHandlers(mux)
}
