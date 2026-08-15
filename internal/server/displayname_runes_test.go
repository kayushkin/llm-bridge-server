package server

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kayushkin/llm-bridge/msg"
)

// A four-byte rune. A two-byte rune leaves only one of the two byte offsets
// inside it, so a test that picks the other one passes against a byte cut.
const fourByteRune = "\U0001D11E" // 𝄞 MUSICAL SYMBOL G CLEF

// slideFourByteRuneAcrossCut runs fn over inputs that place a four-byte rune at
// every byte offset around cutAt, and reports how many of those inputs actually
// straddled the offset a byte cut would use.
//
// It asserts only what must hold at every offset: the result is valid UTF-8 and
// keeps exactly the first cutAt runes. The paddings that put the rune across
// the cut are the ones a byte cut corrupts; the rest are boundary-aligned
// known-negative controls, which pass against the unfixed code too. Without the
// controls we could not tell "detects the straddle" from "rejects non-ASCII".
func slideFourByteRuneAcrossCut(t *testing.T, cutAt int, fn func(string) string, want func(text string) string) (straddles, controls int) {
	t.Helper()
	for padding := cutAt - 3; padding <= cutAt+1; padding++ {
		text := strings.Repeat("a", padding) + fourByteRune + strings.Repeat("b", 120)
		// The rune occupies bytes [padding, padding+4). A byte cut at cutAt
		// lands inside it when padding < cutAt < padding+4.
		straddling := padding < cutAt && cutAt < padding+4
		if straddling {
			straddles++
		} else {
			controls++
		}

		got := fn(text)
		if !utf8.ValidString(got) {
			t.Errorf("padding=%d (straddling=%v): result is not valid UTF-8: %q", padding, straddling, got)
		}
		if w := want(text); got != w {
			t.Errorf("padding=%d (straddling=%v):\n got %q (%d bytes)\nwant %q (%d bytes)",
				padding, straddling, got, len(got), w, len(w))
		}
	}
	return straddles, controls
}

func TestDisplayNameFromMessageNeverSplitsARune(t *testing.T) {
	const maxRunes = 80
	straddles, controls := slideFourByteRuneAcrossCut(t, maxRunes, displayNameFromMessage,
		func(text string) string { return string([]rune(text)[:maxRunes]) + "…" })

	// Guard the instrument. If these inputs ever stop reaching the truncating
	// branch, the test goes green having measured nothing.
	if straddles != 3 {
		t.Fatalf("exercised %d straddling offsets, want 3 — the test no longer reaches the defect", straddles)
	}
	if controls != 2 {
		t.Fatalf("exercised %d boundary-aligned controls, want 2", controls)
	}
}

func TestDisplayNameFromMessageLeavesShortTextAlone(t *testing.T) {
	for _, text := range []string{"hello", "héllo " + fourByteRune, fourByteRune} {
		if got := displayNameFromMessage(text); got != text {
			t.Errorf("displayNameFromMessage(%q) = %q, want it unchanged", text, got)
		}
	}
}

func TestDisplayNameForDiscoveredSessionNeverSplitsARune(t *testing.T) {
	const maxRunes = maxDiscoveredDisplayNameRunes
	straddles, controls := slideFourByteRuneAcrossCut(t, maxRunes,
		func(prompt string) string { return displayNameForDiscoveredSession(prompt, "unused-project") },
		func(text string) string { return string([]rune(text)[:maxRunes]) })

	if straddles != 3 {
		t.Fatalf("exercised %d straddling offsets, want 3 — the test no longer reaches the defect", straddles)
	}
	if controls != 2 {
		t.Fatalf("exercised %d boundary-aligned controls, want 2", controls)
	}
}

// The project directory is the fallback, and it is truncated on a rune boundary
// too — a discovered session with no prompt is the only way this branch is
// reached, so it would otherwise never be exercised.
func TestDisplayNameForDiscoveredSessionFallsBackToProject(t *testing.T) {
	if got := displayNameForDiscoveredSession("a prompt", "a project"); got != "a prompt" {
		t.Errorf("prompt should win: got %q", got)
	}
	if got := displayNameForDiscoveredSession("", "a project"); got != "a project" {
		t.Errorf("empty prompt should fall back to project: got %q", got)
	}

	longProject := strings.Repeat("a", maxDiscoveredDisplayNameRunes-1) + fourByteRune + "tail"
	got := displayNameForDiscoveredSession("", longProject)
	if !utf8.ValidString(got) {
		t.Errorf("project fallback is not valid UTF-8: %q", got)
	}
	if want := string([]rune(longProject)[:maxDiscoveredDisplayNameRunes]); got != want {
		t.Errorf("project fallback: got %q, want %q", got, want)
	}
}

// TestDisplayNameForDiscoveredSessionCapIsExactly100Runes pins the VALUE of
// maxDiscoveredDisplayNameRunes, which the two tests above cannot.
//
// Both of them name the cap through the constant — `cutAt` and the expected
// prefix are both maxDiscoveredDisplayNameRunes — so the input and the
// expectation move together and the pair stays green for any value of it. Moved
// to 99 and to 101, the whole package stayed green. The neighbouring 80 in
// displayNameFromMessage is spelled out as a literal and is caught both ways;
// these two caps read alike and were pinned very differently.
//
// So the literal is written out here, and it has to be a SECOND test rather than
// a stricter assertion inside the first: the first test's job is the rune
// boundary, and it needs the constant to place its straddling paddings.
//
// The last kept rune is marked with '~' rather than the result being measured by
// length — a cut that keeps the wrong end returns the right length. '~' appears
// nowhere else in either input.
func TestDisplayNameForDiscoveredSessionCapIsExactly100Runes(t *testing.T) {
	exactly100 := strings.Repeat("a", 99) + "~"
	if n := utf8.RuneCountInString(exactly100); n != 100 {
		t.Fatalf("fixture is %d runes, want 100 — the test no longer reaches the defect", n)
	}

	if got := displayNameForDiscoveredSession(exactly100, "unused-project"); got != exactly100 {
		t.Errorf("a 100-rune prompt must be served whole:\n got %q\nwant %q", got, exactly100)
	}

	// One rune over. It must come back as exactly the first 100 — still ending
	// in the marker, with the 101st rune gone.
	oneOver := exactly100 + "b"
	if got := displayNameForDiscoveredSession(oneOver, "unused-project"); got != exactly100 {
		t.Errorf("a 101-rune prompt must be cut to 100:\n got %q\nwant %q", got, exactly100)
	}
}

// TestAutoRenameHandlerNeverStoresASplitRune is the call-site test. A helper
// with green tests proves nothing about the places that call it, so this drives
// the real HTTP handler and reads the name back out of the store.
func TestAutoRenameHandlerNeverStoresASplitRune(t *testing.T) {
	// Put the four-byte rune across the byte offset the cut used to use.
	name := strings.Repeat("a", maxAutoRenameRunes-1) + fourByteRune + strings.Repeat("b", 40)

	stored := storeNameViaAutoRenameHandler(t, name)
	if !utf8.ValidString(stored) {
		t.Errorf("stored display_name is not valid UTF-8: %q", stored)
	}
	if want := string([]rune(name)[:maxAutoRenameRunes]); stored != want {
		t.Errorf("stored display_name = %q, want %q", stored, want)
	}
}

// TestAutoRenameCapIsExactly24Runes pins the VALUE of maxAutoRenameRunes, which
// the test above cannot.
//
// That test builds its input from maxAutoRenameRunes and expects
// `[:maxAutoRenameRunes]`, so input and expectation move together: the cap moved
// to 23 and to 25 both left the package green. The case that IS caught there
// multiplies the cap by ten — which asks whether the cap still applies at all, a
// different question from where it sits, and the two are one `*10` apart in the
// same file.
//
// 24 is spelled out here, and the marker rune says which end was kept.
func TestAutoRenameCapIsExactly24Runes(t *testing.T) {
	exactly24 := strings.Repeat("a", 23) + "~"
	if n := utf8.RuneCountInString(exactly24); n != 24 {
		t.Fatalf("fixture is %d runes, want 24 — the test no longer reaches the defect", n)
	}

	if stored := storeNameViaAutoRenameHandler(t, exactly24); stored != exactly24 {
		t.Errorf("a 24-rune title must be stored whole:\n got %q\nwant %q", stored, exactly24)
	}
	if stored := storeNameViaAutoRenameHandler(t, exactly24+"b"); stored != exactly24 {
		t.Errorf("a 25-rune title must be cut to 24:\n got %q\nwant %q", stored, exactly24)
	}
}

// storeNameViaAutoRenameHandler drives the real auto-rename HTTP handler with
// one display name and returns what came to rest in the store. A helper with
// green tests proves nothing about the places that call it, so these tests go
// through the handler rather than calling the truncation directly.
//
// Each call gets its own server and session: ApplyAutoRename is guarded by the
// renamer slot, and reusing a session across two renames would make the second
// call's result depend on the first.
func storeNameViaAutoRenameHandler(t *testing.T, displayName string) string {
	t.Helper()

	srv, st, instID := testServerWithInstance(t, msg.HarnessClaudeCode)
	resp := doJSON(t, srv, "POST", "/sessions", msg.CreateSessionRequest{
		Type:       msg.SessionTypeInteractive,
		Purpose:    msg.PurposeChat,
		Origin:     "test",
		Harness:    "claude_code",
		InstanceID: instID,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create session: status %d", resp.StatusCode)
	}
	created := decodeJSON[msg.ManagedSession](t, resp)

	// The handler verifies the caller still owns the renamer slot, so claim it
	// the way spawnRenamerSession would.
	if ok, err := st.ReserveRenamerSlot(created.SessionID, "renamer-1"); err != nil || !ok {
		t.Fatalf("reserve renamer slot: ok=%v err=%v", ok, err)
	}

	resp = doJSON(t, srv, "POST", "/sessions/"+created.SessionID+"/auto-rename", AutoRenameRequest{
		DisplayName:      displayName,
		RenamerSessionID: "renamer-1",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("auto-rename: status %d, want 200", resp.StatusCode)
	}

	sess, err := st.GetSession(created.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.DisplayName
}
