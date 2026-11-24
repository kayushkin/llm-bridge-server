package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// usage-store has no auth and listens on every interface (~/CLAUDE.md records
// :8185 with no firewall rule), so the length of an error body is the caller's
// to choose unless something bounds it. Measured against the deployed binary
// before the repair: a 5 000-byte occurred_at_str rendered 10 096 bytes,
// because time.Parse quotes its whole input back inside its own error twice.
//
// These cases are written against the handler rather than the mux so they do
// not need a store: the 400 path returns before the store is touched.

// bodyCeiling is the most a 400 for a bad occurred_at_str may render. It is a
// ceiling, not the observed length: it must not move when the prefix or the
// diagnosis is reworded, only when the bound is removed.
const bodyCeiling = 300

func postTopup(t *testing.T, occurredAtStr string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"provider":        "probe",
		"amount_usd":      0,
		"occurred_at_str": occurredAtStr,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/usage/spend/topups", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	(&Server{}).handleAddTopup(rec, req)
	return rec
}

func TestABadOccurredAtStrCannotChooseTheLengthOfItsOwn400(t *testing.T) {
	// Swept rather than sampled: the amplification is a multiple of the input,
	// so a single length cannot tell a bound from a coincidence.
	for _, n := range []int{100, 1000, 5000, 50000} {
		rec := postTopup(t, strings.Repeat("x", n))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%d x's: status %d, want 400", n, rec.Code)
		}
		if got := rec.Body.Len(); got > bodyCeiling {
			t.Errorf("%d x's: 400 body is %d bytes, want <= %d", n, got, bodyCeiling)
		}
	}
}

func TestTheBodyLengthDoesNotGrowWithTheValue(t *testing.T) {
	// The discriminating case. A ceiling alone is satisfied by a bound that is
	// merely generous; this fails unless the length stops tracking the input.
	short := postTopup(t, strings.Repeat("x", 100)).Body.Len()
	long := postTopup(t, strings.Repeat("x", 50000)).Body.Len()
	if long != short {
		t.Errorf("body is %d bytes for 100 x's and %d for 50 000; the caller still chooses the length", short, long)
	}
}

func TestAnOrdinaryBadDateStillSaysWhatIsWrongWithIt(t *testing.T) {
	// The discriminating negative in the other direction: dropping the error
	// entirely would pass every ceiling above and make the endpoint useless.
	rec := postTopup(t, "2026-13-01")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("400 body is not the {\"error\":...} shape: %v", err)
	}
	if !strings.Contains(out["error"], "occurred_at_str") {
		t.Errorf("error = %q, want it to name the field", out["error"])
	}
	if !strings.Contains(out["error"], "month out of range") {
		t.Errorf("error = %q, want it to still carry the parser's diagnosis", out["error"])
	}
}

func TestAWellFormedDateIsNotRejectedForBeingUnparseable(t *testing.T) {
	// The no-change control. Every case above asserts something about a 400, so
	// a repair that rejected every date would satisfy them all.
	//
	// It asserts the reason rather than the status: a well-formed date gets past
	// the parse and is then refused further down for amount_usd being zero, and
	// these cases deliberately do not carry a store to get past that. Reading
	// the reason is what keeps this a control instead of a restatement.
	rec := postTopup(t, "2026-08-30")
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not the {\"error\":...} shape: %v", err)
	}
	if strings.Contains(out["error"], "occurred_at_str") || strings.Contains(out["error"], "parsing time") {
		t.Errorf("a well-formed date was refused by the parse: %q", out["error"])
	}
}
