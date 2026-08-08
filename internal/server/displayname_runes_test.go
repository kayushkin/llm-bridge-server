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

// TestAutoRenameHandlerNeverStoresASplitRune is the call-site test. A helper
// with green tests proves nothing about the places that call it, so this drives
// the real HTTP handler and reads the name back out of the store.
func TestAutoRenameHandlerNeverStoresASplitRune(t *testing.T) {
	// Put the four-byte rune across the byte offset the cut used to use.
	name := strings.Repeat("a", maxAutoRenameRunes-1) + fourByteRune + strings.Repeat("b", 40)

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
		DisplayName:      name,
		RenamerSessionID: "renamer-1",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("auto-rename: status %d, want 200", resp.StatusCode)
	}

	sess, err := st.GetSession(created.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !utf8.ValidString(sess.DisplayName) {
		t.Errorf("stored display_name is not valid UTF-8: %q", sess.DisplayName)
	}
	if want := string([]rune(name)[:maxAutoRenameRunes]); sess.DisplayName != want {
		t.Errorf("stored display_name = %q, want %q", sess.DisplayName, want)
	}
}
