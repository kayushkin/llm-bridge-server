package residencezones

import (
	"testing"
	"time"
)

func TestParseRefusesMovesOutOfOrder(t *testing.T) {
	for _, spec := range []string{"", "Nowhere/Zone", "UTC,2022-01-01", "UTC,2022-05-01=UTC,2022-01-01=UTC"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) took it", spec)
		}
	}
}

func TestAtFollowsTheMoves(t *testing.T) {
	zones, err := Parse("Europe/Lisbon,2021-07-11=America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2021, 7, 10, 23, 0, 0, 0, time.UTC)
	after := time.Date(2021, 7, 11, 0, 0, 0, 0, time.UTC)
	if got := zones.At(before).String(); got != "Europe/Lisbon" {
		t.Errorf("before the move = %s", got)
	}
	if got := zones.At(after).String(); got != "America/Chicago" {
		t.Errorf("at the move = %s", got)
	}
}
