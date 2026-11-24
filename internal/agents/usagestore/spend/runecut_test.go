package spend

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// jsonEscapedReplacementChar is what encoding/json actually writes for an
// invalid UTF-8 byte: the six ASCII characters of the \ufffd escape, NOT the
// encoded U+FFFD rune. Spelled as two raw literals so the distinction survives
// an editor that would otherwise normalize one into the other. The obvious
// assertion — strings.ContainsRune(body, utf8.RuneError) — cannot fail on
// marshalled JSON, so it reads as coverage and is not.
const jsonEscapedReplacementChar = `\u` + `fffd`

// straddlingAdminKey is 28 bytes: "sk-ant-admin0" (13) + U+20AC (3) +
// "1234567" (7) + U+1F600 (4) + "z". Byte 14 — where the 14-byte prefix cut
// falls — is a continuation byte of U+20AC, and byte len-4 is a continuation
// byte of U+1F600, so the unrepaired cut splits a rune at BOTH ends. A fixture
// whose rune widths happened to divide the budgets would land both cuts on a
// boundary and pass against the very code it was written to condemn, so the
// test guards that it still straddles.
const straddlingAdminKey = "sk-ant-admin0€1234567\U0001F600z"

func hintFor(t *testing.T, key string) string {
	t.Helper()
	c := &AnthropicKeyCollector{APIKeyFn: func() (string, error) { return key, nil }}
	got, err := c.adminKeyHint()
	if err != nil {
		t.Fatalf("adminKeyHint(%q): %v", key, err)
	}
	return got
}

func TestAdminKeyHintNeverSplitsARuneAtEitherCut(t *testing.T) {
	const key = straddlingAdminKey

	if len(key) < 24 {
		t.Fatalf("fixture is only %d bytes, so adminKeyHint returns it whole and neither cut runs", len(key))
	}
	if utf8.RuneStart(key[14]) {
		t.Fatalf("fixture does not straddle the PREFIX cut: byte 14 (%#x) starts a rune, so this test would pass against the byte cut", key[14])
	}
	if utf8.RuneStart(key[len(key)-4]) {
		t.Fatalf("fixture does not straddle the SUFFIX cut: byte len-4 (%#x) starts a rune, so this test would pass against the byte cut", key[len(key)-4])
	}

	got := hintFor(t, key)

	if !utf8.ValidString(got) {
		t.Errorf("adminKeyHint(%q) = %q, which is not valid UTF-8", key, got)
	}
	// The byte cut would return "sk-ant-admin0\xe2\x82...\x9f\x98\x80z".
	// Dropping the two partial runes leaves both halves short of their
	// budgets, which is the intended trade: a shorter hint over an invalid one.
	if want := "sk-ant-admin0...z"; got != want {
		t.Errorf("adminKeyHint(%q) = %q, want %q", key, got, want)
	}
}

// TestAdminKeyHintReachesTheWireAsValidUTF8 is the consequence assertion. The
// hint's destination is the admin_key_hint field of GET /api/spend/keys
// (internal/server.ProviderAccount), and encoding/json substitutes U+FFFD for
// invalid UTF-8 rather than returning an error — so the damage is silent and
// lands on the wire, not in this package. The mirror struct below carries the
// same field name and tag; internal/server cannot be imported here because it
// imports this package.
func TestAdminKeyHintReachesTheWireAsValidUTF8(t *testing.T) {
	wire := struct {
		AdminKeyHint string `json:"admin_key_hint"`
	}{hintFor(t, straddlingAdminKey)}

	body, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(body), jsonEscapedReplacementChar) {
		t.Errorf("admin_key_hint carries U+FFFD on the wire: %s", body)
	}
}

// The known-negative control. Every Anthropic admin-key format is ASCII, so
// this is the case that actually runs, and the repair must not move it.
func TestAdminKeyHintIsUnchangedWhenTheCutsAreAlreadyAligned(t *testing.T) {
	const key = "sk-ant-admin01-abcdefghijklmnop"

	if got, want := hintFor(t, key), "sk-ant-admin01...mnop"; got != want {
		t.Errorf("adminKeyHint(%q) = %q, want %q", key, got, want)
	}
}

func TestAdminKeyHintSlidesAFourByteRuneAcrossThePrefixCut(t *testing.T) {
	straddled := 0
	for lead := 10; lead < 18; lead++ {
		key := strings.Repeat("a", lead) + "\U0001F600" + strings.Repeat("b", 24)
		got := hintFor(t, key)

		if !utf8.ValidString(got) {
			t.Errorf("lead=%d: adminKeyHint(%q) = %q, which is not valid UTF-8", lead, key, got)
		}
		if !utf8.RuneStart(key[14]) {
			straddled++
		}
	}
	// Leads 11, 12 and 13 put a continuation byte at index 14.
	if straddled != 3 {
		t.Fatalf("the slide straddled the prefix cut %d times, want 3 — the fixture no longer exercises the defect", straddled)
	}
}

func TestSuffixAtRuneBoundaryDropsThePartialRuneRatherThanOverrunTheBudget(t *testing.T) {
	cases := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"abc\U0001F600", 4, "\U0001F600"}, // aligned: the whole rune fits exactly
		{"abc\U0001F600", 3, ""},           // straddles: no whole rune fits in 3 bytes
		{"ab\U0001F600cd", 4, "cd"},        // walks forward past 3 continuation bytes
		{"abcdef", 0, ""},
		{"abcdef", -1, ""},
		{"ab", 9, "ab"}, // budget longer than the string
	}
	for _, c := range cases {
		if got := suffixAtRuneBoundary(c.in, c.maxBytes); got != c.want {
			t.Errorf("suffixAtRuneBoundary(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
		}
	}
}

func TestTruncateAtRuneBoundaryDropsThePartialRune(t *testing.T) {
	cases := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"\U0001F600abc", 4, "\U0001F600"},
		{"\U0001F600abc", 3, ""},
		{"ab\U0001F600", 4, "ab"},
		{"abcdef", 0, ""},
		{"abcdef", -1, ""},
		{"ab", 9, "ab"},
	}
	for _, c := range cases {
		if got := truncateAtRuneBoundary(c.in, c.maxBytes); got != c.want {
			t.Errorf("truncateAtRuneBoundary(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
		}
	}
}
