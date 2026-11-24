package memorystore

import (
	"math"
	"testing"
)

func TestEmbedDeterminism(t *testing.T) {
	e := NewEmbedder()
	text := "The quick brown fox jumps over the lazy dog"

	v1 := e.Embed(text)
	v2 := e.Embed(text)

	if len(v1) != 256 {
		t.Fatalf("expected vector size 256, got %d", len(v1))
	}
	if len(v1) != len(v2) {
		t.Fatalf("vector length mismatch: %d vs %d", len(v1), len(v2))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Errorf("non-deterministic at index %d: %f vs %f", i, v1[i], v2[i])
		}
	}
}

func TestEmbedL2Normalized(t *testing.T) {
	e := NewEmbedder()
	v := e.Embed("filesystem search code introspection tools")

	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	// Non-empty text should yield a unit vector.
	if math.Abs(norm-1.0) > 1e-9 {
		t.Errorf("expected L2 norm 1.0, got %f", norm)
	}
}

func TestEmbedEmptyText(t *testing.T) {
	e := NewEmbedder()
	v := e.Embed("")
	if len(v) != 256 {
		t.Fatalf("expected vector size 256, got %d", len(v))
	}
	for i, x := range v {
		if x != 0 {
			t.Errorf("expected zero vector for empty text, index %d = %f", i, x)
		}
	}
}

func TestEmbedStopWordsAndShortTokensIgnored(t *testing.T) {
	e := NewEmbedder()
	// All tokens are either stop words or <=2 chars, so nothing survives tokenization.
	v := e.Embed("the and for is a an of")
	for i, x := range v {
		if x != 0 {
			t.Errorf("expected zero vector when only stop/short words present, index %d = %f", i, x)
		}
	}
}

func TestEmbedSimilarTextsAreClose(t *testing.T) {
	e := NewEmbedder()
	a := e.Embed("database connection pooling configuration")
	b := e.Embed("database connection pooling configuration settings")
	c := e.Embed("orange banana apple mango pineapple")

	simAB := CosineSimilarity(a, b)
	simAC := CosineSimilarity(a, c)

	if simAB <= simAC {
		t.Errorf("expected related texts to be more similar: simAB=%f simAC=%f", simAB, simAC)
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("Hello, World! foo123 a an the cat")
	// "Hello"->hello, "World"->world, "foo123" kept; "a","an" too short;
	// "the" stop word; "cat" kept.
	want := map[string]bool{"hello": true, "world": true, "foo123": true, "cat": true}
	if len(got) != len(want) {
		t.Fatalf("token count mismatch: got %v, want %d tokens", got, len(want))
	}
	for _, tok := range got {
		if !want[tok] {
			t.Errorf("unexpected token %q", tok)
		}
	}
}

func TestTokenizeTrailingToken(t *testing.T) {
	// Ensure the final token (no trailing delimiter) is captured.
	got := tokenize("alpha beta gamma")
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHashStringDeterministicAndNonNegative(t *testing.T) {
	if hashString("token") != hashString("token") {
		t.Error("hashString is not deterministic")
	}
	// Verify the known polynomial-rolling hash value.
	// "ab" => 31*97 + 98 = 3105
	if got := hashString("ab"); got != 3105 {
		t.Errorf("hashString(\"ab\") = %d, want 3105", got)
	}
	if hashString("") != 0 {
		t.Error("hashString(\"\") should be 0")
	}
	// A long string that would overflow into negative must be made non-negative.
	for _, s := range []string{"a", "supercalifragilisticexpialidocious", "the-longest-possible-token-name-here-xyz"} {
		if hashString(s) < 0 {
			t.Errorf("hashString(%q) is negative: %d", s, hashString(s))
		}
	}
}

func TestIsStopWord(t *testing.T) {
	for _, w := range []string{"the", "and", "for", "this", "that", "which"} {
		if !isStopWord(w) {
			t.Errorf("expected %q to be a stop word", w)
		}
	}
	for _, w := range []string{"database", "memory", "fox", "embedding"} {
		if isStopWord(w) {
			t.Errorf("expected %q to NOT be a stop word", w)
		}
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1},
		{"orthogonal", []float64{1, 0}, []float64{0, 1}, 0},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, -1},
		{"scaled-equal", []float64{1, 2, 3}, []float64{2, 4, 6}, 1},
		{"length-mismatch", []float64{1, 2}, []float64{1}, 0},
		{"empty", []float64{}, []float64{}, 0},
		{"zero-vector", []float64{0, 0}, []float64{1, 1}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CosineSimilarity(%v, %v) = %f, want %f", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCosineSimilarityKnownAngle(t *testing.T) {
	// 45-degree angle => cos = 1/sqrt(2)
	got := CosineSimilarity([]float64{1, 0}, []float64{1, 1})
	want := 1.0 / math.Sqrt(2)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %f, want %f", got, want)
	}
}
