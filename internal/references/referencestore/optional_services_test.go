package referencestore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

func TestPartialDeploymentStartsWithoutInventedAddresses(t *testing.T) {
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{
		"REFERENCE_STORE_PRINCIPAL_STORE_URL": "http://localhost:12345",
		"REFERENCE_STORE_GRANT_STORE_URL":     "http://localhost:12346",
		"REFERENCE_STORE_SERVICE_TOKEN":       "test-service-token",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReferenceConfiguration(Types, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReferenceConfiguration(Types, map[string]string{"noteboard": "/relative"}, nil); err == nil {
		t.Fatal("relative service address accepted")
	}
	if err := ValidateReferenceConfiguration(Types, nil, map[string]string{"music": "/relative"}); err == nil {
		t.Fatal("relative site address accepted")
	}
}

func TestMissingServiceReportsAnErrorAndDoesNotCacheAMiss(t *testing.T) {
	stores := newStores(t)
	resolver := &Resolver{Types: Types, ServiceURLs: map[string]string{}, HTTPClient: http.DefaultClient,
		Credentials: &Credentials{}, LookupTimeout: time.Second, RecordTTL: time.Minute, MissTTL: time.Minute, Concurrency: 1}
	caller := Caller{Unchecked: true}
	results, errs := resolver.Resolve(context.Background(), caller, []string{"prediction_000001"})
	if len(results["prediction_000001"]) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error, "not configured") {
		t.Fatalf("results=%v errors=%v", results, errs)
	}
	_, _, err := resolver.Attachment(context.Background(), caller, "prediction_000001")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("attachment error=%v", err)
	}
	resolver.ServiceURLs["prediction-store"] = stores.server.URL
	results, errs = resolver.Resolve(context.Background(), caller, []string{"prediction_000001"})
	if len(errs) != 0 || len(results["prediction_000001"]) != 1 {
		t.Fatalf("configuration still treated as a miss: results=%v errors=%v", results, errs)
	}
}

func TestSearchReportsUnconfiguredServicesBesideConfiguredResults(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[{"id":"prediction_000001"}]`)) }))
	defer store.Close()
	resolver := &Resolver{Types: []ReferenceType{*TypeNamed("prediction"), *TypeNamed("person")}, ServiceURLs: map[string]string{"prediction-store": store.URL}, HTTPClient: http.DefaultClient, Credentials: &Credentials{}, LookupTimeout: time.Second}
	mux := http.NewServeMux()
	RegisterHandlers(mux, &Server{Resolver: resolver, ServiceToken: "service-token"})
	server := httptest.NewServer(mux)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/search?q=test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(ServiceTokenHeader, "service-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "not configured") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var answer struct {
		Results []json.RawMessage `json:"results"`
		Errors  []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Results) == 0 || len(answer.Errors) == 0 {
		t.Fatalf("missing partial results or errors: %s", body)
	}
}

func TestMissingPageSiteNeverBecomesALocalPage(t *testing.T) {
	resolver := &Resolver{}
	row := &ReferenceType{Page: "/record?id={id}", PageSite: "music"}
	if got := resolver.pageURL(row, "record_000001", nil); got != "" {
		t.Fatalf("unconfigured external site became %q", got)
	}
}
