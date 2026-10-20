package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestExternalActivityMarkOnlyMovesForward(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, ok, err := st.ExternalActivityMark("br_1"); err != nil || ok {
		t.Fatalf("mark on a new session = (ok %v, err %v), want none", ok, err)
	}
	later := time.Date(2026, 10, 20, 16, 0, 5, 0, time.UTC)
	earlier := later.Add(-500 * time.Millisecond) // "…04.5" against "…05": the text trap
	if err := st.AdvanceExternalActivityMark("br_1", later); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceExternalActivityMark("br_1", earlier); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.ExternalActivityMark("br_1")
	if err != nil || !ok || !got.Equal(later) {
		t.Fatalf("mark = %s (ok %v, err %v), want %s: an earlier time moved it back", got, ok, err, later)
	}
	evenLater := later.Add(1500 * time.Millisecond)
	if err := st.AdvanceExternalActivityMark("br_1", evenLater); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.ExternalActivityMark("br_1"); !got.Equal(evenLater) {
		t.Fatalf("mark = %s, want %s", got, evenLater)
	}
}
