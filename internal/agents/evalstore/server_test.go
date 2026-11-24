package evalstore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) (*httptest.Server, *Runner) {
	t.Helper()
	store := openTestStore(t)
	runner := newTestRunner(store, newFakeCaller(), &fakeUsage{})
	mux := http.NewServeMux()
	RegisterHandlers(mux, store, runner, nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, runner
}

func request(t *testing.T, method, url, body string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, url, response.StatusCode, want, data)
	}
	return data
}

const syntheticSuiteJSON = `{"name":"synthetic filing","task_kind":"email-filing","judge_instructions":"Judge it.",
	"judge_failure_kinds":["wrong_card","no_answer","other","none"],"output_schema":{"type":"object"},"max_tokens":1000,
	"judge_max_tokens":2000,"checker_name":"email-filing","baseline_model_id":"model-original",
	"weekly_calls_source":{"kind":"fixed","calls_per_week":10,"note":"a synthetic figure"}}`

func TestTheAPIRunsASuiteFromCreationToReport(t *testing.T) {
	server, runner := testServer(t)
	base := server.URL

	request(t, http.MethodPost, base+"/suites", `{"name":"x","misspelled_field":1}`, http.StatusBadRequest)
	var suite Suite
	json.Unmarshal(request(t, http.MethodPost, base+"/suites", syntheticSuiteJSON, http.StatusCreated), &suite)

	for _, ref := range []string{"one", "two"} {
		body := mustJSON(map[string]any{"source_kind": SourceKindLLMBridgeOperation, "source_ref": "operation_" + ref, "system_prompt": "File mail.",
			"user_prompt": syntheticFilingPrompt("e-" + ref), "original_model_id": "model-original",
			"original_answer": json.RawMessage(`{"filings":[` + syntheticFiling("e-"+ref, syntheticCardA) + `]}`)})
		request(t, http.MethodPost, base+"/suites/"+suite.ID+"/cases", body, http.StatusCreated)
	}
	listed := string(request(t, http.MethodGet, base+"/suites/"+suite.ID+"/cases", "", http.StatusOK))
	if strings.Contains(listed, "TODAY IS") || strings.Contains(listed, "filings") {
		t.Errorf("the case list carries prompts or answers: %s", listed)
	}
	if one := string(request(t, http.MethodGet, base+"/cases/eval_case_000001", "", http.StatusOK)); !strings.Contains(one, "TODAY IS") {
		t.Errorf("GET /cases/{id} lacks the prompt: %s", one)
	}

	var run Run
	json.Unmarshal(request(t, http.MethodPost, base+"/runs", `{"suite_id":"`+suite.ID+`","candidate_model_ids":["model-original","model-small"],
		"judge_model_ids":["judge-a","judge-b"],"original_answer_model_id":"model-original","case_selection":{"mode":"all"},
		"prediction_id":"prediction_000042"}`, http.StatusCreated), &run)
	request(t, http.MethodPost, base+"/runs/"+run.ID+"/start", "", http.StatusAccepted)
	runner.Wait()
	request(t, http.MethodPost, base+"/runs/"+run.ID+"/start", "", http.StatusConflict)

	var got struct {
		Run
		Progress RunProgress `json:"progress"`
	}
	json.Unmarshal(request(t, http.MethodGet, base+"/runs/"+run.ID, "", http.StatusOK), &got)
	if got.Status != RunStatusCompleted || got.Progress.AnswersRecorded != 4 || got.Progress.VerdictsRecorded != 4 {
		t.Fatalf("run %+v", got)
	}

	answers := string(request(t, http.MethodGet, base+"/runs/"+run.ID+"/answers", "", http.StatusOK))
	if strings.Contains(answers, "filings") {
		t.Error("answers carry their bodies without include_bodies")
	}
	if withBodies := string(request(t, http.MethodGet, base+"/runs/"+run.ID+"/answers?include_bodies=true", "", http.StatusOK)); !strings.Contains(withBodies, "filings") {
		t.Error("include_bodies=true left the bodies out")
	}
	if verdicts := string(request(t, http.MethodGet, base+"/runs/"+run.ID+"/verdicts", "", http.StatusOK)); strings.Contains(verdicts, `"reason":"fine"`) {
		t.Error("verdict reasons, which may quote mail, came without include_bodies")
	}

	var report Report
	json.Unmarshal(request(t, http.MethodGet, base+"/runs/"+run.ID+"/report", "", http.StatusOK), &report)
	if report.Cases != 2 || len(report.Models) != 2 || report.PredictionID != "prediction_000042" || *report.CallsPerWeek != 10 {
		t.Errorf("report %+v", report)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/runs/"+run.ID+"/report?format=markdown&calls_per_week=50", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	markdown, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/markdown") || !strings.Contains(string(markdown), "Calls per week: 50 (given in the request)") {
		t.Errorf("markdown report (%s):\n%s", response.Header.Get("Content-Type"), markdown)
	}

	request(t, http.MethodPatch, base+"/runs/"+run.ID, `{"prediction_id":"not-a-prediction"}`, http.StatusBadRequest)
	request(t, http.MethodDelete, base+"/suites/"+suite.ID, "", http.StatusConflict)
	request(t, http.MethodDelete, base+"/runs/"+run.ID, "", http.StatusOK)
	request(t, http.MethodDelete, base+"/suites/"+suite.ID, "", http.StatusOK)
	request(t, http.MethodGet, base+"/runs/"+run.ID, "", http.StatusNotFound)
}

func TestImportOperationsWithoutABridgeIsABadRequest(t *testing.T) {
	server, _ := testServer(t)
	var suite Suite
	json.Unmarshal(request(t, http.MethodPost, server.URL+"/suites", syntheticSuiteJSON, http.StatusCreated), &suite)
	request(t, http.MethodPost, server.URL+"/suites/"+suite.ID+"/cases/import-operations", `{"correlation_id_prefix":"x"}`, http.StatusBadRequest)
}

func TestStoppingARunThatIsNotGoingIsAConflict(t *testing.T) {
	server, _ := testServer(t)
	request(t, http.MethodPost, server.URL+"/runs/eval_run_000001/stop", "", http.StatusConflict)
}
