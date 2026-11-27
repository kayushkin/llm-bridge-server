package localtools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// The suffix web_fetch appends when, and only when, it cut something off.
const truncationNotice = "\n... (truncated)"

// serveText stands up a server returning body as plain UTF-8 text, so web_fetch
// takes the non-HTML path and the bytes it truncates are the bytes served.
func serveText(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// fetch calls the tool the way the invoke handler does and splits the result
// into the text and whether the truncation notice was appended.
func fetch(t *testing.T, url string, request map[string]any) (text string, wasTruncated bool) {
	t.Helper()
	request["url"] = url
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := WebFetch().Run(context.Background(), string(raw))
	if err != nil {
		t.Fatalf("web_fetch returned an error: %v", err)
	}
	if strings.HasPrefix(out, "error") {
		t.Fatalf("web_fetch reported a failure instead of content: %q", out)
	}
	if cut := strings.TrimSuffix(out, truncationNotice); cut != out {
		return cut, true
	}
	return out, false
}

// The schema advertises max_chars as "Maximum characters to return", so these
// are asserted in characters. The two multi-byte characters below are chosen so
// that a byte cut and a character cut disagree loudly: cutting 100 bytes of
// three-byte text yields 33 characters, not 100.
const (
	threeByteRune = "日"          // 日
	fourByteRune  = "\U0001f600" // 😀
)

// TestWebFetchReturnsAsManyCharactersAsWereAsked slides the cut across a
// four-byte character one byte at a time. Every offset in the run is a byte
// offset a byte-counting cut would happily stop at, and only one of them is a
// character boundary.
func TestWebFetchReturnsAsManyCharactersAsWereAsked(t *testing.T) {
	cases := []struct {
		name     string
		page     string
		maxChars int
		// wantChars is what the schema's promise requires, written as a
		// literal so that editing the code cannot move the expectation with it.
		wantChars int
	}{
		{"four-byte characters, cut mid-run", strings.Repeat(fourByteRune, 40), 10, 10},
		{"four-byte characters, cut one earlier", strings.Repeat(fourByteRune, 40), 9, 9},
		{"four-byte characters, cut one later", strings.Repeat(fourByteRune, 40), 11, 11},
		{"three-byte characters", strings.Repeat(threeByteRune, 40), 10, 10},
		{"mixed widths, cut inside the wide run", threeByteRune + fourByteRune + "abc" + strings.Repeat(threeByteRune, 20), 4, 4},
		{"one character asked for", strings.Repeat(fourByteRune, 40), 1, 1},

		// Controls. ASCII is one byte per character, so a byte-counting cut and
		// a character-counting cut agree exactly. These must stay green when the
		// fix is removed — that is what shows the cases above are aimed at the
		// unit of the count and not merely at truncation working at all.
		{"CONTROL ascii, cut mid-page", strings.Repeat("a", 40), 10, 10},
		{"CONTROL ascii, cut at one character", strings.Repeat("a", 40), 1, 1},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			url := serveText(t, testCase.page)
			text, wasTruncated := fetch(t, url, map[string]any{"max_chars": testCase.maxChars})

			if !wasTruncated {
				t.Fatalf("page of %d characters cut to %d was not reported as truncated",
					utf8.RuneCountInString(testCase.page), testCase.maxChars)
			}
			if got := utf8.RuneCountInString(text); got != testCase.wantChars {
				t.Errorf("asked for %d characters, got %d (%d bytes)",
					testCase.maxChars, got, len(text))
			}
			if !utf8.ValidString(text) {
				t.Errorf("cut at %d characters split a character: % x", testCase.maxChars, text)
			}
			if !strings.HasPrefix(testCase.page, text) {
				t.Errorf("returned text is not a prefix of the page served")
			}
		})
	}
}

// TestWebFetchDefaultsToTheCharacterCountItAdvertises pins the number in the
// schema description ("default 50000") rather than the constant in the code, so
// that editing the constant moves the behavior away from the documented
// contract instead of moving the two together.
func TestWebFetchDefaultsToTheCharacterCountItAdvertises(t *testing.T) {
	const documentedDefault = 50000

	// One character over the documented default, in three-byte characters, so
	// that a byte-counting cut would fire far earlier than a character one.
	page := strings.Repeat(threeByteRune, documentedDefault+1)
	url := serveText(t, page)

	for _, request := range []map[string]any{
		{},                // max_chars absent
		{"max_chars": 0},  // max_chars explicitly zero
		{"max_chars": -1}, // max_chars negative
	} {
		text, wasTruncated := fetch(t, url, request)
		if !wasTruncated {
			t.Fatalf("request %v: a page one character over the default was not truncated", request)
		}
		if got := utf8.RuneCountInString(text); got != documentedDefault {
			t.Errorf("request %v: default returned %d characters, schema advertises %d",
				request, got, documentedDefault)
		}
	}
}

// TestWebFetchLeavesAShortPageAloneAndSaysNothingAboutTruncating covers the
// other side of the boundary: the notice is a claim that content was dropped,
// so it must not appear when none was.
func TestWebFetchLeavesAShortPageAloneAndSaysNothingAboutTruncating(t *testing.T) {
	// Exactly at the limit, in four-byte characters: a byte-counting cut fires
	// here (40 characters are 160 bytes) and a character-counting one must not.
	page := strings.Repeat(fourByteRune, 40)
	url := serveText(t, page)

	text, wasTruncated := fetch(t, url, map[string]any{"max_chars": 40})
	if wasTruncated {
		t.Errorf("a page of exactly max_chars characters was reported as truncated")
	}
	if text != page {
		t.Errorf("a page of exactly max_chars characters came back changed:\n got %q\nwant %q", text, page)
	}
}

// TestTheTruncatedTextSurvivesTheInvokeHandlersEncoding is the reason this file
// asserts on characters rather than on UTF-8 validity. The invoke handler
// returns every local tool's output through json.Marshal, which rewrites
// invalid UTF-8 to U+FFFD without reporting an error — so a split character is
// repaired on the way out and a validity assertion downstream of it can never
// fail. The character count is not repaired, which is what makes it the
// assertion worth having.
func TestTheTruncatedTextSurvivesTheInvokeHandlersEncoding(t *testing.T) {
	page := strings.Repeat(fourByteRune, 40)
	url := serveText(t, page)
	text, _ := fetch(t, url, map[string]any{"max_chars": 10})

	encoded, err := json.Marshal(map[string]string{"output": text})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["output"] != text {
		t.Errorf("the handler's encoding altered the text it was handed:\n got %q\nwant %q",
			decoded["output"], text)
	}
	if got := utf8.RuneCountInString(decoded["output"]); got != 10 {
		t.Errorf("asked for 10 characters, %d arrived at the far side of the handler", got)
	}
}
