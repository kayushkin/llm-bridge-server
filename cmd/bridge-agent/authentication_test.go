package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGatedServer stands in for llm-bridge-server's gate: POST /sessions
// answers 401 to a request that carries neither the service token nor a
// bearer token, and records the credential and body each create arrived with.
type fakeGatedServer struct {
	createdWithToken      string
	createdAsHeader       string
	createdWithBearer     string
	createdWithPrincipal  string
	demoLoginWasRequested bool
}

func (f *fakeGatedServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/demo-login", func(w http.ResponseWriter, r *http.Request) {
		f.demoLoginWasRequested = true
		http.Error(w, `{"error":{"code":"demo_login_disabled"}}`, http.StatusNotFound)
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(serviceTokenHeader)
		bearer, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" && !hasBearer {
			http.Error(w, `{"error":{"code":"not_logged_in"}}`, http.StatusUnauthorized)
			return
		}
		var body struct {
			PrincipalID string `json:"principal_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("create body: %v", err)
		}
		if hasBearer && body.PrincipalID != "" {
			http.Error(w, `{"error":{"code":"delegate_principal_mismatch"}}`, http.StatusForbidden)
			return
		}
		f.createdWithToken = token
		f.createdAsHeader = r.Header.Get(principalIDHeader)
		f.createdWithBearer = bearer
		f.createdWithPrincipal = body.PrincipalID
		_, _ = w.Write([]byte(`{"session_id":"br_test"}`))
	})
	return mux
}

func TestASessionAgentCreatesItsDelegateWithItsPostingToken(t *testing.T) {
	fake := &fakeGatedServer{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	d, err := authenticatedDelegate(srv.URL, "", "posting-token-value", "")
	if err != nil {
		t.Fatalf("authenticatedDelegate: %v", err)
	}
	if _, err := d.createSession(context.Background(), "claude_code", "", "", "delegate", "hello", map[string]any{}, true); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if fake.createdWithBearer != "posting-token-value" {
		t.Errorf("create carried bearer %q, want the posting token", fake.createdWithBearer)
	}
	if fake.createdWithToken != "" || fake.createdAsHeader != "" {
		t.Errorf("a posting-token caller sent service headers: token=%q principal=%q", fake.createdWithToken, fake.createdAsHeader)
	}
	if fake.demoLoginWasRequested {
		t.Error("the CLI asked for a demo login; it must never")
	}
}

func TestAPrincipalTheServerRefusesIsReportedVerbatim(t *testing.T) {
	srv := httptest.NewServer((&fakeGatedServer{}).handler(t))
	defer srv.Close()

	d, err := authenticatedDelegate(srv.URL, "", "posting-token-value", "principal_000002")
	if err != nil {
		t.Fatalf("authenticatedDelegate: %v", err)
	}
	_, err = d.createSession(context.Background(), "claude_code", "", "", "delegate", "hello", map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "delegate_principal_mismatch") {
		t.Fatalf("err = %v, want the server's delegate_principal_mismatch", err)
	}
}

func TestAServiceTokenIsSentWithThePrincipalItActsAs(t *testing.T) {
	fake := &fakeGatedServer{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	d, err := authenticatedDelegate(srv.URL, "service-token-value", "posting-token-value", "principal_000001")
	if err != nil {
		t.Fatalf("authenticatedDelegate: %v", err)
	}
	if _, err := d.createSession(context.Background(), "claude_code", "", "", "delegate", "hello", map[string]any{}, true); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if fake.createdWithToken != "service-token-value" || fake.createdAsHeader != "principal_000001" {
		t.Errorf("create carried token=%q principal=%q, want the token and principal_000001", fake.createdWithToken, fake.createdAsHeader)
	}
	if fake.createdWithBearer != "" {
		t.Errorf("a service-token caller also sent bearer %q", fake.createdWithBearer)
	}
}

func TestNoCredentialStopsBeforeCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	_, err := authenticatedDelegate(srv.URL, "", "", "principal_000001")
	if err == nil || !strings.Contains(err.Error(), sessionPostingTokenEnvironmentVariable) || !strings.Contains(err.Error(), serviceTokenEnvironmentVariable) {
		t.Fatalf("err = %v, want one naming both %s and %s", err, sessionPostingTokenEnvironmentVariable, serviceTokenEnvironmentVariable)
	}
	if called {
		t.Error("the server was called with no credential")
	}
}
