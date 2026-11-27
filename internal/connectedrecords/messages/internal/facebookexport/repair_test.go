package facebookexport

import "testing"

func TestRepairLeavesTextThatIsNotMisencoded(t *testing.T) {
	for _, text := range []string{"plain", "Zoë", "日本"} {
		if got := Repair(text); got != text {
			t.Errorf("Repair(%q) = %q", text, got)
		}
	}
}

func TestRepairUndoesLatin1EscapedUTF8(t *testing.T) {
	if got := Repair("ZoÃ«"); got != "Zoë" {
		t.Errorf("Repair = %q, want Zoë", got)
	}
}
