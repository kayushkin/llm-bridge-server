package photolookup

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhotoIDMatchesByHashAndLeavesPrivateDocumentsUnlinked(t *testing.T) {
	hashOf := func(text string) string {
		digest := sha256.Sum256([]byte(text))
		return hex.EncodeToString(digest[:])
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("sha256") {
		case hashOf("beach picture"):
			w.Write([]byte(`[{"id":"photo_000007","tags":["beach"]}]`))
		case hashOf("passport"):
			w.Write([]byte(`[{"id":"photo_000008","tags":["private-document"]}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	lookup := New(server.URL)
	for original, want := range map[string]string{"beach picture": "photo_000007", "passport": "", "unknown": ""} {
		got, err := lookup.PhotoID([]byte(original))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("PhotoID(%q) = %q, want %q", original, got, want)
		}
	}
	if lookup.Found != 1 || lookup.PrivateDocuments != 1 || lookup.NotFound != 1 {
		t.Errorf("counts = %d found, %d private, %d not found", lookup.Found, lookup.PrivateDocuments, lookup.NotFound)
	}
}
