package memorystore

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// newTestServer builds a real mux over a throwaway store, so these tests go
// through the same handler a live caller reaches rather than through Save.
func newTestServer(t *testing.T) *http.ServeMux {
	t.Helper()

	dbPath := "/tmp/test_provenance_" + uuid.New().String() + ".db"
	t.Cleanup(func() { os.Remove(dbPath) })

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	return mux
}

// saveViaHTTP posts body to POST /memories and returns the stored id.
func saveViaHTTP(t *testing.T, mux *http.ServeMux, body map[string]any) string {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/memories", bytes.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("save returned %d: %s", rec.Code, rec.Body.String())
	}

	var saved struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	// The handler echoes the id the caller supplied; these tests always supply
	// one. Whether an absent id should be refused or minted is noteboard todo
	// a475c73c and deliberately not settled here.
	if saved.ID == "" {
		t.Fatal("save returned an empty id")
	}
	return saved.ID
}

// getViaHTTP reads a memory back through GET /memories/{id}.
func getViaHTTP(t *testing.T, mux *http.ServeMux, id string) Memory {
	t.Helper()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/memories/"+id, nil))
	if rec.Code != 200 {
		t.Fatalf("get returned %d: %s", rec.Code, rec.Body.String())
	}

	var m Memory
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	return m
}

// A caller that names no source must not be recorded as the user. "user" is the
// top of the trust order this field exists to express, so defaulting to it
// turns "nobody said" into the strongest possible claim about provenance.
func TestSaveWithNoSourceDoesNotClaimTheUserSaidIt(t *testing.T) {
	mux := newTestServer(t)

	id := saveViaHTTP(t, mux, map[string]any{
		"id":      uuid.New().String(),
		"content": "a memory whose provenance nobody stated",
	})

	got := getViaHTTP(t, mux, id).Source
	if got == "user" {
		t.Fatal("an omitted source was recorded as \"user\": unknown provenance must not be promoted to the most trusted value")
	}
	if got != "" {
		t.Fatalf("an omitted source should stay empty, got %q", got)
	}
}

// The complement, and the reason the assertion above is about "user" and not
// merely about emptiness: a caller that does state its provenance must have it
// stored verbatim. Without this, dropping the field entirely would pass.
func TestSaveKeepsTheSourceACallerActuallyGave(t *testing.T) {
	mux := newTestServer(t)

	for _, source := range []string{"user", "agent", "system", "compaction", "extraction"} {
		id := saveViaHTTP(t, mux, map[string]any{
			"id":      uuid.New().String(),
			"content": "a memory from " + source,
			"source":  source,
		})

		if got := getViaHTTP(t, mux, id).Source; got != source {
			t.Errorf("source %q was stored as %q", source, got)
		}
	}
}

// Importance still defaults, and that is not an inconsistency: 0.5 is the
// middle of importance's range and asserts nothing, while the trust order has
// no middle. This pins the distinction so the next reader does not "restore
// symmetry" by giving Source a default back.
func TestImportanceStillDefaultsEvenThoughSourceDoesNot(t *testing.T) {
	mux := newTestServer(t)

	id := saveViaHTTP(t, mux, map[string]any{
		"id":      uuid.New().String(),
		"content": "a memory with neither importance nor source",
	})

	// Reading a memory boosts its importance by 1.01 (crud.go, Get), so the
	// value that comes back through the API is 0.505, not 0.5. Assert the band
	// rather than the exact number: the point is that importance defaulted at
	// all, and pinning 0.5 here would be a test of the access boost.
	m := getViaHTTP(t, mux, id)
	if m.Importance < 0.5 || m.Importance > 0.51 {
		t.Errorf("importance should still default to ~0.5, got %v", m.Importance)
	}
	if m.Source != "" {
		t.Errorf("source should not default, got %q", m.Source)
	}
}
