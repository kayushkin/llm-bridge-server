package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An error body must not grow with a value the caller sent. ed9280e bounded
// eight such sites in internal/model; a probe on 2026-10-05 found the shape had
// come back in the gate, in board, column and card routes, and in the clients
// that check an id with another store. Each case below answered about 50 000
// bytes to a 50 000-byte value before the fix. The two gate cases need no
// credentials at all: X-Principal-Id is what an unauthenticated caller sends.
const errorBodyCeiling = 300

var callerValue = strings.Repeat("a", 50000)

func TestAnErrorBodyDoesNotCarryTheCallersWholeValue(t *testing.T) {
	h, _, _ := setupWithPrincipalEnforcement(t)
	created := requestAs(t, h, asService, "POST", "/api/boards", map[string]any{"name": "bounds"})
	mustStatus(t, created, http.StatusCreated, "create board")
	var board struct{ ID string }
	if err := json.Unmarshal(created.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	column := requestAs(t, h, asService, "POST", "/api/boards/"+board.ID+"/columns", map[string]any{"name": "bounds"})
	mustStatus(t, column, http.StatusCreated, "create column")
	var col struct{ ID string }
	if err := json.Unmarshal(column.Body.Bytes(), &col); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		site    string
		headers map[string]string
		method  string
		path    string
		body    any
	}{
		{"gate: malformed X-Principal-Id", asPrincipal(callerValue), "GET", "/api/boards", nil},
		{"gate: unknown X-Principal-Id of the right shape", asPrincipal("principal_" + strings.Repeat("9", 50000)), "GET", "/api/boards", nil},
		{"board default_principal_id", asService, "PATCH", "/api/boards/" + board.ID, map[string]any{"default_principal_id": callerValue}},
		{"board organization_id", asService, "PATCH", "/api/boards/" + board.ID, map[string]any{"organization_id": callerValue}},
		{"board default_bundle_id", asService, "PATCH", "/api/boards/" + board.ID, map[string]any{"default_bundle_id": callerValue}},
		{"board default_instance_id", asService, "PATCH", "/api/boards/" + board.ID, map[string]any{"default_instance_id": callerValue}},
		{"column lifecycle_state", asService, "PATCH", "/api/columns/" + col.ID, map[string]any{"lifecycle_state": callerValue}},
		{"column work_state", asService, "PATCH", "/api/columns/" + col.ID, map[string]any{"work_state": callerValue}},
		{"card id on PATCH", asService, "PATCH", "/api/cards/" + callerValue, map[string]any{"title": "x"}},
		{"card id on hold", asService, "POST", "/api/cards/" + callerValue + "/hold", map[string]any{"reason": "x"}},
		{"card id on DELETE", asService, "DELETE", "/api/cards/" + callerValue, nil},
	} {
		w := requestAs(t, h, tc.headers, tc.method, tc.path, tc.body)
		if w.Code < 400 {
			t.Errorf("%s: answered %d; the case has stopped reaching an error", tc.site, w.Code)
			continue
		}
		if n := w.Body.Len(); n > errorBodyCeiling {
			t.Errorf("%s: %d body is %d bytes against a ceiling of %d; the caller is choosing its length\n\tgot: %.200q…",
				tc.site, w.Code, n, errorBodyCeiling, w.Body.String())
		}
	}
}
