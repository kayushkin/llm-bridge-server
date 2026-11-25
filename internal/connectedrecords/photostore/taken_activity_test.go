package photostore

import (
	"fmt"
	"net/http"
	"testing"
)

func TestTakenActivityCountsPerSpanInTheZoneAskedAndLeavesExcludedTagsOut(t *testing.T) {
	service := newTestService(t)
	// 2024-03-01 07:30 UTC is still February 29th in Los Angeles.
	undated := photoOf("e", 0)
	undated.DateSource = DateUnknown
	if status, body := service.call(http.MethodPost, "/imports", ImportRequest{Photos: []ImportPhoto{
		photoOf("a", 1709278200), photoOf("b", 1709300000), photoOf("c", 1709400000), photoOf("d", 1712000000), undated,
	}}, nil); status != http.StatusOK {
		t.Fatalf("import = %d %s", status, body)
	}
	service.call(http.MethodPatch, "/photos/photo_000004", map[string]any{"tags": []string{"private-document"}}, nil)

	var buckets []TakenActivityBucket
	status, body := service.call(http.MethodGet, "/taken-activity?interval=month&time_zone=America/Los_Angeles&samples=1&exclude_tag=private-document", nil, &buckets)
	if status != http.StatusOK {
		t.Fatalf("GET /taken-activity = %d %s", status, body)
	}
	want := "[{1706774400 1 [photo_000001]} {1709280000 2 [photo_000003]}]"
	if fmt.Sprint(buckets) != want {
		t.Errorf("buckets = %v, want %v: February holds the photo taken on the 29th in Los Angeles, April's tagged photo is excluded, and an undated photo is not counted", buckets, want)
	}

	var listed []Photo
	service.call(http.MethodGet, "/photos?exclude_tag=private-document", nil, &listed)
	if len(listed) != 4 {
		t.Errorf("GET /photos?exclude_tag= listed %d photos, want 4", len(listed))
	}

	for _, path := range []string{"/taken-activity?interval=month", "/taken-activity?interval=fortnight&time_zone=UTC", "/taken-activity?interval=day&time_zone=UTC&samples=99"} {
		if status, _ := service.call(http.MethodGet, path, nil, nil); status != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, status)
		}
	}
}

func TestEvenlySpacedKeepsTheEndsAndOrder(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g"}
	if got := fmt.Sprint(evenlySpaced(ids, 3)); got != "[a d g]" {
		t.Errorf("evenlySpaced(7, 3) = %s", got)
	}
	if got := fmt.Sprint(evenlySpaced(ids[:2], 5)); got != "[a b]" {
		t.Errorf("evenlySpaced(2, 5) = %s", got)
	}
}
