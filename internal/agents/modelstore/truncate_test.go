package modelstore

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// The defect these tests pin: cutting a Go string at a fixed byte offset splits
// whatever rune straddles that offset, and the result is not valid UTF-8.
// Nothing on this path reports it — the split rune travels inside an error
// value to the ms CLI, which prints it, and the terminal shows a replacement
// character.
//
// A single hand-picked input is not enough. Whether a cut splits a rune depends
// on where that rune sits relative to the budget, so one string lands off the
// boundary and passes against the unfixed code. Every test here slides the cut
// across a range and counts how much of that range exercised the case.

// musicalNote is four bytes (U+1D11E). A two-byte rune is a weaker probe: only
// one of its two interior offsets is wrong, so a test that picks the other one
// is green against the bug.
const musicalNote = "\U0001D11E"

// TestTruncateAtRuneBoundaryWithEllipsisSlidesTheCutAcrossEveryOffset walks
// maxBytes across an all-four-byte string. Three offsets in every four straddle
// a rune; the fourth is rune-aligned and is the known-negative control, which
// must be a plain byte cut against fixed and unfixed code alike. Without that
// control a green run cannot tell "detects the straddle" from "detects
// non-ASCII input".
func TestTruncateAtRuneBoundaryWithEllipsisSlidesTheCutAcrossEveryOffset(t *testing.T) {
	s := strings.Repeat(musicalNote, 60) // 240 bytes, past the 200-byte budget
	straddled, aligned := 0, 0

	for maxBytes := 1; maxBytes < len(s); maxBytes++ {
		got := truncateAtRuneBoundaryWithEllipsis(s, maxBytes)

		body := strings.TrimSuffix(got, "...")
		if body == got {
			t.Fatalf("maxBytes=%d: input is over budget, so the result must carry the ellipsis; got %q", maxBytes, got)
		}
		if !utf8.ValidString(body) {
			t.Fatalf("maxBytes=%d: kept text is not valid UTF-8: %q", maxBytes, body)
		}
		if len(body) > maxBytes {
			t.Fatalf("maxBytes=%d: kept %d bytes, over budget", maxBytes, len(body))
		}
		// Validity alone is not a falsifiable assertion — a helper returning ""
		// for everything is always valid and always within budget. Pin
		// maximality too: whatever was dropped must genuinely not have fitted.
		_, width := utf8.DecodeRuneInString(s[len(body):])
		if len(body)+width <= maxBytes {
			t.Fatalf("maxBytes=%d: stopped at %d bytes but the next rune (%d bytes) still fits",
				maxBytes, len(body), width)
		}

		if maxBytes%4 == 0 {
			aligned++
			if body != s[:maxBytes] {
				t.Fatalf("maxBytes=%d is rune-aligned, so the walk-back must be a no-op; kept %d bytes", maxBytes, len(body))
			}
			continue
		}
		straddled++
		if body == s[:maxBytes] {
			t.Fatalf("maxBytes=%d straddles a rune, so the cut must move; it did not", maxBytes)
		}
	}

	// The reach-guard: if the fixture ever stops covering both cases, the loop
	// above proves nothing and would still pass.
	if straddled == 0 || aligned == 0 {
		t.Fatalf("fixture covered %d straddling and %d aligned offsets; both must be non-zero", straddled, aligned)
	}
}

// TestTruncBodySlidesARuneAcrossTheProviderBudget is the call-site test. The
// helper being right does not prove truncBody uses it, and truncBody is the
// only caller: all three provider sync paths build their error out of it, so
// this is the shape a real 4xx body arrives in — mostly ASCII JSON with a
// multi-byte rune somewhere in the message field.
func TestTruncBodySlidesARuneAcrossTheProviderBudget(t *testing.T) {
	const budget = 200 // truncBody's own, hardcoded; not read from the code under test
	straddled := 0

	for offset := budget - 3; offset <= budget; offset++ {
		const prefix = `{"error":{"message":"` // 21 bytes, so the rune starts at exactly offset
		body := []byte(prefix + strings.Repeat("a", offset-len(prefix)) + musicalNote + `too long"}}`)
		if len(body) <= budget {
			t.Fatalf("offset=%d: fixture is %d bytes and never reaches the cut", offset, len(body))
		}

		got := truncBody(body)
		kept := strings.TrimSuffix(got, "...")

		if !utf8.ValidString(kept) {
			t.Fatalf("offset=%d: error body is not valid UTF-8 after truncation: %q", offset, kept)
		}
		// The error text is what a user sees. Assert on the formatted message,
		// not just the helper's return, because that is where a split rune
		// actually surfaces.
		msg := fmt.Errorf("HTTP %d: %s", 429, got).Error()
		if !utf8.ValidString(msg) {
			t.Fatalf("offset=%d: formatted error is not valid UTF-8: %q", offset, msg)
		}

		if offset < budget {
			straddled++
			if len(kept) != offset {
				t.Fatalf("offset=%d: the rune straddles the cut, so it must back up to %d bytes; kept %d",
					offset, offset, len(kept))
			}
			continue
		}
		// offset == budget: the rune starts exactly at the budget, so nothing
		// straddles and the cut does not move. The known-negative control.
		if len(kept) != budget {
			t.Fatalf("offset=%d: nothing straddles the cut, so it must not move; kept %d bytes", offset, len(kept))
		}
	}

	if straddled != 3 {
		t.Fatalf("expected 3 straddling placements of a four-byte rune, exercised %d", straddled)
	}
}

// TestTruncBodyLeavesAShortBodyAlone pins the branch that must not gain an
// ellipsis, and the edge the byte cut it replaced panicked on: in source a
// panic and a split rune are the same expression, so no scan for one sees the
// other.
func TestTruncBodyLeavesAShortBodyAlone(t *testing.T) {
	short := []byte(`{"error":"` + musicalNote + `"}`)
	if got := truncBody(short); got != string(short) {
		t.Fatalf("a body inside the budget must come back unchanged; got %q", got)
	}
	if got := truncBody(nil); got != "" {
		t.Fatalf("an empty body must stay empty; got %q", got)
	}
	if got := truncateAtRuneBoundaryWithEllipsis(musicalNote, -1); got != "..." {
		t.Fatalf("a negative budget must keep no text rather than panic; got %q", got)
	}
}
