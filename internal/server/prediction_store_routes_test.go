package server

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictions"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// enablePredictionStoreOnTestDatabase opens the module on a Postgres database
// of the test's own and serves it from server.
func enablePredictionStoreOnTestDatabase(t *testing.T, server *Server) *predictions.Store {
	t.Helper()
	module, err := predictions.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	store := predictions.New(database.Open(module))
	server.EnablePredictions(store)
	return store
}

func (gated *gatedTestServer) requestAsServiceWithJSON(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(requestcaller.ServiceTokenHeader, testServiceToken)
	return serve(gated.server, request)
}

func TestPredictionStoreRoutesAnswer503WithNoPostgres(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	response := gated.requestAsService(t, "GET", "/predictions/calibration")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "prediction_store_off") {
		t.Fatalf("GET /predictions/calibration with no module = %d %s, want 503 prediction_store_off", response.Code, response.Body)
	}
}

// The store's own routes, behind the prefix, answer as the standalone store
// did: the same bodies and the same refusals.
func TestPredictionStoreRoutesServeTheStoreUnderThePrefix(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePredictionStoreOnTestDatabase(t, gated.server)

	created := gated.requestAsServiceWithJSON(t, "POST", "/predictions/predictions", map[string]any{
		"claim": "the cache halves start time", "resolution_criteria": "time ten starts before and after",
		"category": "will-fix", "provenance": "inferred", "probability": 0.7, "tags": []string{"cache"},
	})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"id":"prediction_000001"`) {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	if response := gated.requestAsServiceWithJSON(t, "POST", "/predictions/predictions/prediction_000001/estimates",
		map[string]any{"probability": 0.9, "rationale": "measured", "provenance": "measured"}); response.Code != http.StatusCreated {
		t.Fatalf("estimate = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsServiceWithJSON(t, "POST", "/predictions/predictions/prediction_000001/estimates",
		map[string]any{"probability": 0.73}); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "ladder") {
		t.Fatalf("off-ladder estimate = %d %s, want 400 naming the ladder", response.Code, response.Body)
	}
	if response := gated.requestAsServiceWithJSON(t, "POST", "/predictions/predictions/prediction_000001/links",
		map[string]any{"entity_type": "note", "entity_ref": "f1db12e8-6857-46d9-80ae-9bc4e0d7c200"}); response.Code != http.StatusCreated {
		t.Fatalf("link = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsServiceWithJSON(t, "POST", "/predictions/predictions/prediction_000001/resolve",
		map[string]any{"outcome": "true", "note": "halved"}); response.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", response.Code, response.Body)
	}
	if response := gated.requestAsServiceWithJSON(t, "POST", "/predictions/predictions/prediction_000001/resolve",
		map[string]any{"outcome": "false"}); response.Code != http.StatusConflict {
		t.Fatalf("second resolve = %d %s, want 409", response.Code, response.Body)
	}

	read := gated.requestAsService(t, "GET", "/predictions/predictions/prediction_000001")
	var prediction predictions.Prediction
	if err := json.Unmarshal(read.Body.Bytes(), &prediction); err != nil || read.Code != http.StatusOK {
		t.Fatalf("read = %d %s (%v)", read.Code, read.Body, err)
	}
	if prediction.Probability != 0.9 || prediction.InitialProbability != 0.7 || prediction.Outcome != "true" ||
		len(prediction.Estimates) != 2 || len(prediction.Links) != 1 || prediction.Brier == nil {
		t.Fatalf("read back %+v", prediction)
	}

	calibration := gated.requestAsService(t, "GET", "/predictions/calibration?group_by=category")
	var report predictions.CalibrationReport
	if err := json.Unmarshal(calibration.Body.Bytes(), &report); err != nil || calibration.Code != http.StatusOK {
		t.Fatalf("calibration = %d %s (%v)", calibration.Code, calibration.Body, err)
	}
	if report.Overall.N != 1 || len(report.Groups) != 1 || report.Groups[0].Group != "will-fix" {
		t.Fatalf("calibration = %s", calibration.Body)
	}
	if found := gated.requestAsService(t, "GET", "/predictions/predictions?q=halves"); !strings.Contains(found.Body.String(), `"total":1`) {
		t.Fatalf("search = %d %s", found.Code, found.Body)
	}
	if response := gated.requestAsService(t, "DELETE", "/predictions/links/1"); response.Code != http.StatusOK {
		t.Fatalf("delete link = %d %s", response.Code, response.Body)
	}
}

// The ledger has no owner on a row, so only the operator reaches it: the
// service token, an administrator, or an administrator's session's agent.
func TestPredictionStoreRoutesAreTheOperators(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePredictionStoreOnTestDatabase(t, gated.server)

	if response := gated.requestAs(t, nil, "GET", "/predictions/calibration", nil); response.Code != http.StatusUnauthorized {
		t.Errorf("no credential = %d %s, want 401", response.Code, response.Body)
	}
	person := gated.loginAs(t, firstTestPrincipalID)
	if response := gated.requestAs(t, person, "GET", "/predictions/calibration", nil); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "operator_route") {
		t.Errorf("a person who is not an administrator = %d %s, want 403 operator_route", response.Code, response.Body)
	}
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	if response := gated.requestAs(t, administrator, "GET", "/predictions/calibration", nil); response.Code != http.StatusOK {
		t.Errorf("an administrator = %d %s, want 200", response.Code, response.Body)
	}

	administratorsSession := gated.createSessionAs(t, administrator, administratorTestPrincipalID)
	if response := gated.requestWithBearer(t, gated.sessionAgentTokenFor(t, administratorsSession), "GET", "/predictions/base-rates"); response.Code != http.StatusOK {
		t.Errorf("an administrator's session agent token = %d %s, want 200", response.Code, response.Body)
	}
	personsSession := gated.createSessionAs(t, person, firstTestPrincipalID)
	if response := gated.requestWithBearer(t, gated.sessionAgentTokenFor(t, personsSession), "GET", "/predictions/base-rates"); response.Code != http.StatusUnauthorized {
		t.Errorf("another person's session agent token = %d %s, want 401", response.Code, response.Body)
	}
}

// standalonePredictionStoreCopy is a copy of a standalone prediction-store's
// SQLite file, made with sqlite3 .backup, to check an import against:
//
//	go test ./internal/server -run TestImportedStandaloneDatabaseCalibration -args \
//	  -standalone-prediction-store-copy=/tmp/copy.db -calibration-output=/tmp/calibration.json
//
// The test imports the copy into a fresh test database and writes what
// GET /predictions/calibration answers for it, for a comparison with the
// standalone store's own GET /calibration.
var (
	standalonePredictionStoreCopy = flag.String("standalone-prediction-store-copy", "", "a copy of a standalone prediction-store database to import")
	calibrationOutput             = flag.String("calibration-output", "", "where to write the calibration report of the imported copy")
)

func TestImportedStandaloneDatabaseCalibration(t *testing.T) {
	if *standalonePredictionStoreCopy == "" || *calibrationOutput == "" {
		t.Skip("no -standalone-prediction-store-copy and -calibration-output given")
	}
	gated := newGatedTestServer(t, nil)
	module, err := predictions.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	db := database.Open(module)
	counts, err := predictions.ImportStandaloneDatabase(context.Background(), db, *standalonePredictionStoreCopy)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	t.Logf("imported %+v", counts)
	gated.server.EnablePredictions(predictions.New(db))
	report := map[string]json.RawMessage{}
	for _, path := range []string{"/predictions/calibration", "/predictions/calibration?group_by=category", "/predictions/base-rates", "/predictions/health", "/predictions/predictions?include_deleted=1&expand=1", "/predictions/tags"} {
		response := gated.requestAsService(t, "GET", path)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, response.Code, response.Body)
		}
		report[path] = json.RawMessage(response.Body.Bytes())
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*calibrationOutput, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
