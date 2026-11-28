package possessions

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every write route the other tests leave out, once each, so each of their
// statements runs against Postgres.
func TestTheRemainingRoutesWriteAndReadBack(t *testing.T) {
	store := openTestStore(t)
	mux := http.NewServeMux()
	registerEveryRoute(mux, &Reviewer{Store: store, Photos: fakePhotos{}})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		asTestOwner(mux).ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	expect := func(want int, method, path, body string) string {
		t.Helper()
		got := call(method, path, body)
		if got.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, got.Code, got.Body, want)
		}
		return got.Body.String()
	}
	expect(201, "POST", "/possessions", `{"actor":"principal_000001","name":"bike","vehicle":{"vehicle_type":"bicycle"}}`)
	expect(200, "POST", "/possessions/possession_000001/photos", `{"photo_id":"photo_000001","box_left":0.1,"box_top":0.1,"box_right":0.5,"box_bottom":0.5,"linked_by":"principal_000001"}`)
	if body := expect(200, "POST", "/possessions/possession_000001/photos", `{"photo_id":"photo_000002","linked_by":"principal_000001"}`); !strings.Contains(body, `"photo_000002"`) {
		t.Fatalf("second photo not linked: %s", body)
	}
	expect(200, "DELETE", "/possessions/possession_000001/photos/photo_000002", "")
	expect(201, "POST", "/vehicles/vehicle_000001/odometer-readings", `{"read_at":1000,"miles":12,"source":"principal_000001"}`)
	expect(409, "POST", "/vehicles/vehicle_000001/odometer-readings", `{"read_at":1000,"miles":13,"source":"principal_000001"}`)
	expect(204, "DELETE", "/possessions/possession_000001?actor=principal_000001", "")
	expect(404, "GET", "/possessions/possession_000001", "")
	expect(200, "POST", "/possessions/possession_000001/restore", `{"actor":"principal_000001"}`)
	if body := expect(200, "GET", "/possessions/possession_000001/revisions", ""); !strings.Contains(body, `"owner_principal_id":"principal_000001"`) {
		t.Fatalf("revisions = %s", body)
	}
	expect(200, "GET", "/products?q=metronome", "")
	expect(201, "POST", "/purchases", `{"seller":"a shop","marketplace":"in_store","ordered_at":1000,"source":"manual","lines":[{"title":"bell","quantity":1}]}`)
	expect(201, "POST", "/purchases", `{"seller":"a shop","marketplace":"amazon","order_reference":"A-1","ordered_at":1000,"source":"manual","lines":[{"title":"bell"}]}`)
	expect(409, "POST", "/purchases", `{"seller":"a shop","marketplace":"amazon","order_reference":"A-1","ordered_at":1000,"source":"manual","lines":[{"title":"bell"}]}`)
	if body := expect(200, "GET", "/purchases?marketplace=amazon", ""); !strings.Contains(body, `"A-1"`) {
		t.Fatalf("purchases = %s", body)
	}
	expect(204, "PATCH", "/purchase-lines/purchase_line_000001", `{"disposition":"consumable","actor":"principal_000001"}`)

	expect(201, "POST", "/photo-reviews", `{"photo_id":"photo_000001","queued_by":"principal_000001","reading":"inventory"}`)
	expect(200, "POST", "/photo-reviews", `{"photo_id":"photo_000001","queued_by":"principal_000001","reading":"inventory"}`)
	if body := expect(200, "GET", "/photo-reviews?status=queued", ""); !strings.Contains(body, `"photo_review_000001"`) {
		t.Fatalf("queued reviews = %s", body)
	}
	if err := store.setReviewStatus("photo_review_000001", "reading", "thumbnail", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.RequeueInterrupted(); err != nil {
		t.Fatal(err)
	}
	if review, err := store.GetPhotoReview("photo_review_000001"); err != nil || review.Status != "queued" {
		t.Fatalf("an interrupted review = %+v %v, want queued again", review, err)
	}
	if err := store.setReviewStatus("photo_review_000001", "failed", "", "made-up failure"); err != nil {
		t.Fatal(err)
	}
	expect(200, "POST", "/photo-reviews/photo_review_000001/retry", `{"actor":"principal_000001"}`)

	expect(201, "POST", "/possessions", `{"actor":"principal_000001","name":"shirt","garment":{"category":"top"}}`)
	expect(201, "POST", "/outfits", `{"actor":"principal_000001","status":"kept","possession_ids":["possession_000002"]}`)
	expect(201, "POST", "/outfits/outfit_000001/wearings", `{"worn_on":"2026-01-02"}`)
	expect(204, "DELETE", "/outfits/outfit_000001?actor=principal_000001", "")
	expect(404, "GET", "/outfits/outfit_000001", "")
}
