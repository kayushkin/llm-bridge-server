package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPictureSentInTwoMessagesIsIndexedUnderBoth(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sightings" || r.URL.Query().Get("source_machine") != photoSourceMachine {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"photo_id": "photo_000001", "source_path": "message_000001", "media_type": "image/jpeg", "tags": []string{}},
			{"photo_id": "photo_000001", "source_path": "message_000009", "media_type": "image/jpeg", "tags": []string{}},
			{"photo_id": "photo_000002", "source_path": "message_000003", "media_type": "image/jpeg", "tags": []string{"private-document"}},
		})
	}))
	defer store.Close()
	index, err := runner{photoStoreURL: store.URL}.photoIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(index["message_000001"]) != 1 || len(index["message_000009"]) != 1 || index["message_000009"][0].ID != "photo_000001" {
		t.Fatalf("index = %+v", index)
	}
	if len(index["message_000003"]) != 0 {
		t.Fatalf("a private document was indexed: %+v", index)
	}
}
