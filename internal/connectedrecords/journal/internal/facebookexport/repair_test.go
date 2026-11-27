package facebookexport

import "testing"

func TestRepairLeavesTextThatIsNotMisencoded(t *testing.T) {
	for _, text := range []string{"plain", "Zoë", "日本"} {
		if got := Repair(text); got != text {
			t.Errorf("Repair(%q) = %q", text, got)
		}
	}
}
