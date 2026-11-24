package textutil

import (
	"math/rand"
	"strings"
	"testing"
)

// Differential: over random strings, this function must agree with strings.Title
// EXCEPT where a flanked ASCII apostrophe appears.
func TestDifferentialAgainstStringsTitle(t *testing.T) {
	alphabet := []rune("abcXY01_-. /'’éǆ ")
	r := rand.New(rand.NewSource(42))
	agree, differ := 0, 0
	for i := 0; i < 200000; i++ {
		n := r.Intn(8)
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteRune(alphabet[r.Intn(len(alphabet))])
		}
		in := sb.String()
		got := TitleFirstRuneOfEachWord(in)
		want := strings.Title(in)
		if got == want {
			agree++
			continue
		}
		differ++
		if !hasFlankedApostrophe(in) {
			t.Fatalf("diverged with no flanked apostrophe: %q -> got %q want %q", in, got, want)
		}
	}
	t.Logf("agree=%d differ=%d", agree, differ)
	if differ == 0 {
		t.Fatal("no divergence at all — the corpus never exercised the fix")
	}
}

func hasFlankedApostrophe(s string) bool {
	rs := []rune(s)
	for i := 1; i < len(rs)-1; i++ {
		if rs[i] == '\'' && isWordRune(rs[i-1]) && isWordRune(rs[i+1]) {
			return true
		}
	}
	return false
}
