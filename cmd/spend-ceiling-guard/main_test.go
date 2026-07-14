package main

import "testing"

// A card without a ceiling must never be held. This feature is opt-in, and an
// auto-hold that arms itself on work nobody capped would silently stop the whole
// queue.
func TestNoCeilingMeansNoEnforcement(t *testing.T) {
	for name, item := range map[string]map[string]any{
		"key absent":   {"title": "x"},
		"explicit nil": {"title": "x", "auto_hold_at_usd": nil},
	} {
		if _, ok := spendCeiling(item); ok {
			t.Errorf("%s: reported a ceiling where none was set", name)
		}
	}
}

// Zero is a real ceiling ("hold before spending a cent"), not the absence of
// one. If these collapse, either every uncapped card gets held or every
// $0-capped card runs free.
func TestZeroCeilingIsARealCeiling(t *testing.T) {
	got, ok := spendCeiling(map[string]any{"auto_hold_at_usd": 0.0})
	if !ok {
		t.Fatal("a ceiling of $0 read as 'no ceiling' — zero and unset have collapsed")
	}
	if got != 0 {
		t.Fatalf("ceiling = %v, want 0", got)
	}
}

func TestCeilingIsReadFromTheItem(t *testing.T) {
	got, ok := spendCeiling(map[string]any{"auto_hold_at_usd": 12.5})
	if !ok || got != 12.5 {
		t.Fatalf("spendCeiling = (%v, %v), want (12.5, true)", got, ok)
	}
}
