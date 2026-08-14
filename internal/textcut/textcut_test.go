// Tests for textcut.
//
// Three rules govern the assertions here, each learned by scoring an earlier
// suite against deliberate sabotage rather than by reading it:
//
//   - Sweep the alignment, do not pick one. A fixture that repeats a single wide
//     rune from offset zero cannot land inside a rune at a bound that its width
//     divides — 60 is divisible by 2, 3 and 4, which is every rune width Go has,
//     so such a fixture is green against a byte cut. Every sweep below shifts the
//     fixture by 0, 1 and 2 ASCII bytes.
//
//   - Assert the cut POSITION, not the returned length, where the bug being
//     pinned is an off-by-one in the length. Writing the expected length means
//     writing maxRunes+1 the moment the code is one over, which records the
//     defect as the contract.
//
//   - Measure what survives the wire. A display name is JSON-encoded on its way
//     out, and encoding/json replaces invalid bytes with U+FFFD and returns no
//     error, so utf8.ValidString on a value read back off the wire is green
//     whatever happened. What survives laundering is the rune COUNT and the
//     U+FFFD blot, so those are what the round-trip test looks at.
package textcut

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// wideRunes covers every multi-byte width, plus ASCII as the control. The one
// that matters most is the 3-byte rune: 3 divides 60, so a fixture of nothing
// but 日 aligns perfectly with a byte cut at the limit and hides the defect.
var wideRunes = []struct {
	name  string
	rune_ string
	bytes int
}{
	{"ascii", "a", 1},
	{"latin1-accent", "é", 2},
	{"japanese", "日", 3},
	{"emoji", "🟢", 4},
}

// shifts slides the wide runes across the cut point. Any single one of these is
// half a test: which alignments expose a byte cut depends on the rune width, and
// no single shift exposes all of them.
var shifts = []int{0, 1, 2}

// distinctRunesOfWidth builds a string of count runes, each one different and
// each encoding to the given number of bytes. Use it where the assertion is
// about WHICH characters came back — a fixture of one repeated rune agrees with
// any cut that keeps the right number of them, whatever it dropped.
func distinctRunesOfWidth(bytes, count int) string {
	// Each base starts a run of consecutive code points that all encode to the
	// given width: ASCII capitals, Latin-1 accented letters, CJK ideographs, the
	// emoticon block. The span is how far that run goes before the width changes,
	// so the sequence repeats rather than widening — ASCII has only 26 capitals.
	// A repeat is fine here; what the fixture has to avoid is a period of one.
	base := map[int]rune{1: 'A', 2: 'À', 3: '一', 4: '😀'}[bytes]
	span := map[int]int{1: 26, 2: 64, 3: 512, 4: 512}[bytes]
	out := make([]rune, count)
	for i := range out {
		out[i] = base + rune(i%span)
	}
	return string(out)
}

// The fixture above is only worth anything if it really produces runes of the
// width it promises, and really varies. A fixture nobody checked is the thing
// three of these tests exist to distrust.
func TestTheFixtureItselfIsWhatItClaimsToBe(t *testing.T) {
	for _, w := range wideRunes {
		s := distinctRunesOfWidth(w.bytes, 200)
		if n := utf8.RuneCountInString(s); n != 200 {
			t.Errorf("width %d: fixture holds %d runes, want 200", w.bytes, n)
		}
		if len(s) != 200*w.bytes {
			t.Errorf("width %d: fixture is %d bytes, want %d — a rune left the width band",
				w.bytes, len(s), 200*w.bytes)
		}
		if runes := []rune(s); runes[0] == runes[1] {
			t.Errorf("width %d: fixture repeats a single rune, so it cannot tell a shortened value from a shifted one", w.bytes)
		}
	}
}

func TestTruncateToRuneLimit_LeavesAValueThatAlreadyFitsAlone(t *testing.T) {
	for _, w := range wideRunes {
		s := strings.Repeat(w.rune_, 10)
		if got := TruncateToRuneLimit(s, 60); got != s {
			t.Errorf("%s: TruncateToRuneLimit(%q, 60) = %q, want it returned unchanged", w.name, s, got)
		}
	}
}

// The bound is inclusive: a value of exactly maxRunes characters is not over it,
// so nothing may be cut and no ellipsis may appear. This is the off-by-one that
// a >= would introduce, and it is invisible in every over-long fixture.
func TestTruncateToRuneLimit_AValueOfExactlyTheLimitIsUntouched(t *testing.T) {
	for _, w := range wideRunes {
		s := strings.Repeat(w.rune_, 60)
		if got := TruncateToRuneLimit(s, 60); got != s {
			t.Errorf("%s: a value of exactly 60 runes came back changed: %d runes, %q…",
				w.name, utf8.RuneCountInString(got), got[:min(len(got), 20)])
		}
	}
}

// The headline defect: the bound is a character count, so a non-ASCII value must
// keep as many characters as an ASCII one. A byte cut keeps 22 of 60 Japanese
// characters and 18 of 60 emoji.
func TestTruncateToRuneLimit_KeepsTheSameNumberOfCharactersWhateverTheirWidth(t *testing.T) {
	for _, w := range wideRunes {
		for _, shift := range shifts {
			s := strings.Repeat("a", shift) + strings.Repeat(w.rune_, 200)
			got := TruncateToRuneLimit(s, 60)
			if n := utf8.RuneCountInString(got); n != 60 {
				t.Errorf("%s shift=%d: kept %d characters of a 60-character bound (%d bytes)",
					w.name, shift, n, len(got))
			}
		}
	}
}

// The ellipsis is part of the bound, not an addition to it. Reserving one byte
// for a three-byte rune — or reserving nothing — returns a value over the bound
// its caller asked for.
func TestTruncateToRuneLimit_NeverReturnsMoreCharactersThanTheLimit(t *testing.T) {
	for _, w := range wideRunes {
		for _, shift := range shifts {
			for _, limit := range []int{1, 2, 7, 59, 60, 61} {
				s := strings.Repeat("a", shift) + strings.Repeat(w.rune_, 200)
				got := TruncateToRuneLimit(s, limit)
				if n := utf8.RuneCountInString(got); n > limit {
					t.Errorf("%s shift=%d limit=%d: returned %d characters, over the limit: %q",
						w.name, shift, limit, n, got)
				}
			}
		}
	}
}

// A cut that lands inside a rune yields invalid UTF-8 in memory. Sweeping the
// alignment is what finds it; one fixture probes one alignment.
func TestTruncateToRuneLimit_NeverCutsARuneInHalf(t *testing.T) {
	for _, w := range wideRunes {
		for _, shift := range shifts {
			for limit := 1; limit <= 64; limit++ {
				s := strings.Repeat("a", shift) + strings.Repeat(w.rune_, 200)
				got := TruncateToRuneLimit(s, limit)
				if !utf8.ValidString(got) {
					t.Errorf("%s shift=%d limit=%d: produced invalid UTF-8: %q", w.name, shift, limit, got)
				}
			}
		}
	}
}

// The kept part is a prefix of the input: a cut may shorten a value, never
// rewrite one. This is what makes "60 characters" mean the FIRST 60.
//
// The fixture here is DISTINCT runes rather than one repeated, and that is the
// whole test. Against strings.Repeat("a", n), a cut that drops the first
// character and keeps one more — s[1:i] — is still a prefix of the input, so a
// repeated-rune fixture cannot tell "shortened" from "shifted". Scored: with the
// repeated fixture this test was silent on exactly that sabotage.
func TestTruncateToRuneLimit_KeepsThePrefixItClaimsToKeep(t *testing.T) {
	for _, w := range wideRunes {
		for _, shift := range shifts {
			s := strings.Repeat("a", shift) + distinctRunesOfWidth(w.bytes, 200)
			got := TruncateToRuneLimit(s, 60)
			kept := strings.TrimSuffix(got, Ellipsis)
			if !strings.HasPrefix(s, kept) {
				t.Errorf("%s shift=%d: kept text is not a prefix of the input: %q", w.name, shift, kept)
			}
			// And the cut lands exactly where the character count runs out —
			// asserted as a POSITION, so this test cannot be satisfied by
			// writing down whatever length the code happens to return.
			wantKept := 60 - utf8.RuneCountInString(Ellipsis)
			if n := utf8.RuneCountInString(kept); n != wantKept {
				t.Errorf("%s shift=%d: cut after %d characters, want after %d (the limit less the ellipsis)",
					w.name, shift, n, wantKept)
			}
		}
	}
}

// A shortened value says so. Without this, returning the first 60 characters and
// dropping the rest silently passes every bound assertion above.
func TestTruncateToRuneLimit_MarksAValueItShortened(t *testing.T) {
	for _, w := range wideRunes {
		got := TruncateToRuneLimit(strings.Repeat(w.rune_, 200), 60)
		if !strings.HasSuffix(got, Ellipsis) {
			t.Errorf("%s: shortened value does not end in the ellipsis: %q", w.name, got)
		}
	}
}

// A limit with no room for anything is not a panic and not a value over the
// bound. Ranging to maxRunes-1 with maxRunes == 0 would index s[:-1].
func TestTruncateToRuneLimit_ALimitOfZeroOrLessYieldsNothing(t *testing.T) {
	for _, limit := range []int{0, -1, -60} {
		if got := TruncateToRuneLimit("日本語のとても長い質問", limit); got != "" {
			t.Errorf("limit=%d: returned %q, want the empty string", limit, got)
		}
	}
}

// What the caller actually ships is the JSON-encoded value, and encoding/json
// launders invalid bytes into U+FFFD with a nil error — so the round trip is
// where a mid-rune cut stops being loud. The two things that survive laundering
// are the character count and the replacement character, so both are asserted.
func TestTruncateToRuneLimit_SurvivesTheJSONEncodingItIsAboutToGoThrough(t *testing.T) {
	for _, w := range wideRunes {
		for _, shift := range shifts {
			s := strings.Repeat("a", shift) + strings.Repeat(w.rune_, 200)
			encoded, err := json.Marshal(map[string]string{"display_name": TruncateToRuneLimit(s, 60)})
			if err != nil {
				t.Fatalf("%s shift=%d: marshal: %v", w.name, shift, err)
			}
			var back map[string]string
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("%s shift=%d: unmarshal: %v", w.name, shift, err)
			}
			got := back["display_name"]
			if strings.ContainsRune(got, '�') {
				t.Errorf("%s shift=%d: a replacement character reached the wire: %q", w.name, shift, got)
			}
			if n := utf8.RuneCountInString(got); n != 60 {
				t.Errorf("%s shift=%d: %d characters reached the wire, want 60", w.name, shift, n)
			}
		}
	}
}
