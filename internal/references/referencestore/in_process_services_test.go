package referencestore

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A service in this process is asked through its handler, under its path
// prefix, with no credential of the kind it took over HTTP, and an
// owner-decided type still names the caller so the owner narrows.
func TestAnInProcessServiceIsAskedThroughItsHandler(t *testing.T) {
	mux := http.NewServeMux()
	asked := 0
	mux.HandleFunc("GET /prediction-store/predictions/{id}", func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.PathValue("id") != "prediction_000001" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":"prediction_000001","claim":"it rains"}`))
	})
	mux.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-LLM-Bridge-Service-Token") != "" {
			t.Error("an in-process request carried a credential")
		}
		if r.Header.Get(PrincipalIDHeader) != "principal_000002" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"session_id":"` + r.PathValue("id") + `","display_name":"a chat"}`))
	})
	resolver := &Resolver{
		Types:       Types,
		InProcess:   InProcessServices{Handler: mux, PathPrefixes: map[string]string{"prediction-store": "/prediction-store", "llm-bridge-server": ""}},
		Credentials: &Credentials{},
		Access:      &AccessChecker{Directory: fakeAccess{}, TTL: time.Minute},
		RecordTTL:   time.Minute, MissTTL: time.Minute, LookupTimeout: time.Second, Concurrency: 2,
	}
	results, errs := resolver.Resolve(context.Background(), Caller{PrincipalID: "principal_000002"},
		[]string{"prediction_000001", "prediction_000002", "br_1794083694355010889"})
	if len(errs) != 0 {
		t.Fatalf("errors: %+v", errs)
	}
	if m := results["prediction_000001"]; len(m) != 1 || m[0].Refused || m[0].Label != "it rains" {
		t.Errorf("the prediction: %+v", m)
	}
	if m := results["prediction_000002"]; len(m) != 0 {
		t.Errorf("a missing prediction: %+v", m)
	}
	if m := results["br_1794083694355010889"]; len(m) != 1 || m[0].Refused || m[0].Label != "a chat" {
		t.Errorf("the caller's session: %+v", m)
	}
	other, errs := resolver.Resolve(context.Background(), Caller{PrincipalID: "principal_000003"}, []string{"br_1794083694355010889"})
	if len(errs) != 0 || len(other["br_1794083694355010889"]) != 0 {
		t.Errorf("another principal's session reads as unknown: %+v %+v", other, errs)
	}
	if asked != 2 {
		t.Errorf("prediction-store was asked %d times, want 2", asked)
	}
}
