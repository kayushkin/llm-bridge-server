package spend

import (
	"encoding/json"
	"strings"
	"testing"
)

// shortAdminKey is 20 bytes, below adminKeyHint's 24-byte guard, so it takes
// the branch this file exists to pin. It is spelled to look like a real
// credential — a placeholder or truncated key is exactly the case that reaches
// the short branch in practice — and it shares no substring with the masked
// answer, so an assertion against it cannot pass by accident.
const shortAdminKey = "sk-ant-admin01-trunc"

// maskedShortAdminKey is written as a literal rather than built with
// strings.Repeat, which is the expression adminKeyHint itself uses. An expected
// value derived from the code under test drifts in step with it and can never
// fail for a wrong number of stars.
const maskedShortAdminKey = "********************"

func TestAdminKeyHintMasksAKeyTooShortToCut(t *testing.T) {
	if len(shortAdminKey) != 20 {
		t.Fatalf("fixture is %d bytes; the test's literal expectation assumes 20", len(shortAdminKey))
	}
	if len(shortAdminKey) >= 24 {
		t.Fatalf("fixture is %d bytes, so adminKeyHint cuts it and the short branch never runs", len(shortAdminKey))
	}
	if len(maskedShortAdminKey) != len(shortAdminKey) {
		t.Fatalf("expectation is %d stars for a %d-byte fixture", len(maskedShortAdminKey), len(shortAdminKey))
	}

	got := hintFor(t, shortAdminKey)

	// The property that actually failed: the hint was the secret itself. This
	// assertion is the one that must survive any later change to what a masked
	// hint looks like.
	if got == shortAdminKey {
		t.Errorf("adminKeyHint returned the key unmasked: %q", got)
	}
	if got != maskedShortAdminKey {
		t.Errorf("adminKeyHint(%q) = %q, want %q", shortAdminKey, got, maskedShortAdminKey)
	}
}

// TestAdminKeyHintNeverEchoesTheKeyAtAnyShortLength walks every length the
// short branch can see. A single-length fixture would pin one row of the branch
// and leave the rest free to echo; the defect was a property of the branch, not
// of one length.
func TestAdminKeyHintNeverEchoesTheKeyAtAnyShortLength(t *testing.T) {
	for n := 1; n < 24; n++ {
		key := "sk-ant-admin01-abcdefgh"[:n]

		got := hintFor(t, key)

		if got == key {
			t.Errorf("len=%d: adminKeyHint returned the key unmasked: %q", n, got)
		}
		if strings.ContainsAny(got, "sk-antdmi0123456789abcdefgh") {
			t.Errorf("len=%d: adminKeyHint(%q) = %q, which carries characters of the key", n, key, got)
		}
	}
}

// TestAdminKeyHintMasksRatherThanCutsAtTheThresholdItself straddles the 24-byte
// guard from both sides in one test. A test that only ever supplied a short key
// could not notice the threshold moving down and taking real keys with it.
func TestAdminKeyHintMasksRatherThanCutsAtTheThresholdItself(t *testing.T) {
	const justUnder = "sk-ant-admin01-abcdefgh" // 23 bytes
	const justOver = "sk-ant-admin01-abcdefghi" // 24 bytes

	if len(justUnder) != 23 || len(justOver) != 24 {
		t.Fatalf("fixtures are %d and %d bytes, want 23 and 24 — they no longer straddle the guard",
			len(justUnder), len(justOver))
	}

	if got, want := hintFor(t, justUnder), "***********************"; got != want {
		t.Errorf("23 bytes: adminKeyHint(%q) = %q, want %q", justUnder, got, want)
	}
	// One byte longer and the cut runs, so the hint is identifiable again. This
	// is the known-negative side: the repair must not swallow the case the
	// function exists to serve.
	if got, want := hintFor(t, justOver), "sk-ant-admin01...fghi"; got != want {
		t.Errorf("24 bytes: adminKeyHint(%q) = %q, want %q", justOver, got, want)
	}
}

// TestAShortAdminKeyDoesNotReachTheWire is the consequence assertion. The hint
// is published as the admin_key_hint field of GET /api/spend/keys on :8185, so
// an unmasked short key is served as JSON by a running service rather than
// merely returned. internal/server cannot be imported here because it imports
// this package, so the mirror struct below carries the same field name and tag.
func TestAShortAdminKeyDoesNotReachTheWire(t *testing.T) {
	wire := struct {
		AdminKeyHint string `json:"admin_key_hint"`
	}{hintFor(t, shortAdminKey)}

	body, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(body), shortAdminKey) {
		t.Errorf("the admin key is served whole on :8185: %s", body)
	}
}
