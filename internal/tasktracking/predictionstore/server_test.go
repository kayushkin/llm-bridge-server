package predictionstore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	s := newTestStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func samplePayload() map[string]any {
	return map[string]any{
		"claim":               "PATCH with an unknown field returns 400",
		"resolution_criteria": "curl a PATCH with a misspelled key at a throwaway job; read the status",
		"category":            "behavior",
		"provenance":          "inferred",
		"probability":         0.85,
		"author":              "claude-code",
		"tags":                []string{"scheduler"},
	}
}

func TestPostCreatesAndGetReadsBack(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := do(t, srv, "POST", "/predictions", samplePayload())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	var created Prediction
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.ID != "prediction_000001" {
		t.Fatalf("id = %q", created.ID)
	}
	status, body = do(t, srv, "GET", "/predictions/"+created.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	status, _ = do(t, srv, "GET", "/predictions/prediction_999999", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// The number and the outcome each have exactly one write path, and the refusal
// names it. A silent no-op here would leave a caller believing the odds moved.
func TestPatchingTheNumberOrTheOutcomeIsRefusedAndNamesTheRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	for field, wantRoute := range map[string]string{
		"probability": "/estimates",
		"outcome":     "/resolve",
		"status":      "/resolve",
	} {
		status, body := do(t, srv, "PATCH", "/predictions/"+created.ID,
			map[string]any{field: "0.9"})
		if status != http.StatusBadRequest {
			t.Fatalf("PATCH %s: status = %d, want 400: %s", field, status, body)
		}
		if !strings.Contains(string(body), wantRoute) {
			t.Fatalf("PATCH %s: error should name %s, got %s", field, wantRoute, body)
		}
	}

	// The number really did not move.
	_, body = do(t, srv, "GET", "/predictions/"+created.ID, nil)
	var after Prediction
	json.Unmarshal(body, &after)
	if after.Probability != 0.85 || after.Status != "open" {
		t.Fatalf("prediction changed: %v %q", after.Probability, after.Status)
	}
}

func TestMisspelledFieldIsRejectedRatherThanDropped(t *testing.T) {
	srv, _ := newTestServer(t)
	payload := samplePayload()
	payload["probabilty"] = 0.85
	delete(payload, "probability")
	status, body := do(t, srv, "POST", "/predictions", payload)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "probabilty") {
		t.Fatalf("error should name the offending key, got %s", body)
	}
}

func TestOffLadderProbabilityIsFourHundred(t *testing.T) {
	srv, _ := newTestServer(t)
	payload := samplePayload()
	payload["probability"] = 0.73
	status, body := do(t, srv, "POST", "/predictions", payload)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "ladder") {
		t.Fatalf("error should explain the ladder, got %s", body)
	}
}

func TestMissingResolutionCriteriaIsFourHundred(t *testing.T) {
	srv, _ := newTestServer(t)
	payload := samplePayload()
	payload["resolution_criteria"] = ""
	status, body := do(t, srv, "POST", "/predictions", payload)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
}

func TestResolvingTwiceIsAConflict(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	status, body := do(t, srv, "POST", "/predictions/"+created.ID+"/resolve",
		map[string]any{"outcome": "false", "note": "measured live; the field was dropped"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	status, _ = do(t, srv, "POST", "/predictions/"+created.ID+"/resolve",
		map[string]any{"outcome": "true"})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
}

func TestEstimateTrailIsServedInOrder(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	status, body := do(t, srv, "POST", "/predictions/"+created.ID+"/estimates",
		map[string]any{"probability": 0.3, "rationale": "read the row back", "provenance": "measured"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	status, body = do(t, srv, "GET", "/predictions/"+created.ID+"/estimates", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var payload struct {
		Estimates []*Estimate `json:"estimates"`
	}
	json.Unmarshal(body, &payload)
	if len(payload.Estimates) != 2 {
		t.Fatalf("estimates = %d, want 2", len(payload.Estimates))
	}
	if payload.Estimates[0].Probability != 0.85 || payload.Estimates[1].Probability != 0.3 {
		t.Fatalf("trail out of order: %+v", payload.Estimates)
	}
}

func TestVocabularyIsServedNotInferred(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := do(t, srv, "GET", "/vocabulary", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var v struct {
		Categories        []string  `json:"categories"`
		Provenances       []string  `json:"provenances"`
		GroupBy           []string  `json:"group_by"`
		ProbabilityLadder []float64 `json:"probability_ladder"`
	}
	json.Unmarshal(body, &v)
	if len(v.Categories) != len(Categories) || len(v.Provenances) != 4 {
		t.Fatalf("vocabulary = %+v", v)
	}
	if len(v.GroupBy) != len(GroupByFields) {
		t.Fatalf("group_by = %v", v.GroupBy)
	}
	// Served empty-store, so nothing is inferred from the rows.
	if len(v.ProbabilityLadder) == 0 {
		t.Fatal("ladder should be served even with no predictions")
	}
}

func TestCalibrationAndBaseRatesAnswerOnAnEmptyStore(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/calibration", "/base-rates", "/calibration?group_by=category"} {
		status, body := do(t, srv, "GET", path, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200: %s", path, status, body)
		}
	}
	status, body := do(t, srv, "GET", "/calibration?group_by=astrology", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
}

func TestLinksAttachAndDetach(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	status, body := do(t, srv, "POST", "/predictions/"+created.ID+"/links",
		map[string]any{"entity_type": "note", "entity_ref": "f1db12e8-6857-46d9-80ae-9bc4e0d7c200", "label": "the todo"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	var link Link
	json.Unmarshal(body, &link)

	status, body = do(t, srv, "GET", "/predictions/"+created.ID+"/links", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "f1db12e8") {
		t.Fatalf("links = %d %s", status, body)
	}
	status, _ = do(t, srv, "DELETE", "/links/"+itoa(link.ID), nil)
	if status != http.StatusOK {
		t.Fatalf("delete link: status = %d", status)
	}
	status, _ = do(t, srv, "DELETE", "/links/"+itoa(link.ID), nil)
	if status != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404", status)
	}
}

func TestDeleteIsReversibleAndHardDeleteIsNot(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	do(t, srv, "DELETE", "/predictions/"+created.ID, nil)
	status, _ := do(t, srv, "POST", "/predictions/"+created.ID+"/restore", nil)
	if status != http.StatusOK {
		t.Fatalf("restore: status = %d", status)
	}
	status, _ = do(t, srv, "DELETE", "/predictions/"+created.ID+"?hard=true", nil)
	if status != http.StatusOK {
		t.Fatalf("hard delete: status = %d", status)
	}
	status, _ = do(t, srv, "GET", "/predictions/"+created.ID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("purged prediction still readable: status = %d", status)
	}
}

func TestHealthCountsOverdue(t *testing.T) {
	srv, store := newTestServer(t)
	payload := samplePayload()
	payload["due_at"] = now() + 86400
	_, created := do(t, srv, "POST", "/predictions", payload)
	var born struct {
		ID string `json:"id"`
	}
	json.Unmarshal(created, &born)
	ageDueDate(t, store, born.ID, 1)

	status, body := do(t, srv, "GET", "/health", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var health struct {
		Status string         `json:"status"`
		Counts map[string]int `json:"counts"`
	}
	json.Unmarshal(body, &health)
	if health.Status != "ok" || health.Counts["overdue"] != 1 {
		t.Fatalf("health = %s", body)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(json.Number(jsonNumber(n)).String())
}

func jsonNumber(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// A map decode makes DisallowUnknownFields a no-op, so PATCH checks its keys by
// hand. Without that check this returns 200 having changed nothing.
func TestMisspelledPatchKeyIsRefusedRatherThanIgnored(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	status, body := do(t, srv, "PATCH", "/predictions/"+created.ID,
		map[string]any{"clam": "a typo for claim"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "clam") || !strings.Contains(string(body), "claim") {
		t.Fatalf("error should name the typo and the accepted keys, got %s", body)
	}
}

// Correcting a note is a PATCH; recording one is part of resolving. Both work,
// and neither can move the outcome.
func TestResolutionNoteIsPatchableOnceResolved(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	if status, body := do(t, srv, "POST", "/predictions/"+created.ID+"/resolve",
		map[string]any{"outcome": "false", "note": "the field was dropped"}); status != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200: %s", status, body)
	}

	status, body := do(t, srv, "PATCH", "/predictions/"+created.ID,
		map[string]any{"resolution_note": "the field was dropped by the OLD binary; rebuilt since"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var patched Prediction
	json.Unmarshal(body, &patched)
	if !strings.Contains(patched.ResolutionNote, "OLD binary") {
		t.Fatalf("note = %q, want the correction", patched.ResolutionNote)
	}
	if patched.Outcome != "false" {
		t.Fatalf("outcome = %q, want false — patching a note must not move it", patched.Outcome)
	}
}

// Refused on an open row, and the message names the route that does settle it.
func TestPatchingTheNoteOfAnOpenPredictionIsFourHundred(t *testing.T) {
	srv, _ := newTestServer(t)
	_, body := do(t, srv, "POST", "/predictions", samplePayload())
	var created Prediction
	json.Unmarshal(body, &created)

	status, body := do(t, srv, "PATCH", "/predictions/"+created.ID,
		map[string]any{"resolution_note": "it went fine"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "resolve") {
		t.Fatalf("error should name the resolve route, got %s", body)
	}
}

// The INSERT writes ” over this field, so accepting it silently discarded the
// caller's prose and answered 201 as though it had landed.
func TestResolutionNoteAtCreationIsRefusedRatherThanDiscarded(t *testing.T) {
	srv, _ := newTestServer(t)
	payload := samplePayload()
	payload["resolution_note"] = "it will go fine"

	status, body := do(t, srv, "POST", "/predictions", payload)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if !strings.Contains(string(body), "resolve") {
		t.Fatalf("error should name the resolve route, got %s", body)
	}
}
