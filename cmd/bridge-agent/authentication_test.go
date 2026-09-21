package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGatedServer stands in for llm-bridge-server's gate: POST /auth/demo-login
// is open and sets a cookie, and POST /sessions answers 401 to a request that
// carries neither that cookie nor the service token. It records the
// credential each create arrived with.
type fakeGatedServer struct {
	loggedInAs       string
	createdWithToken string
	createdAsHeader  string
	createdByCookie  string
}

func (f *fakeGatedServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/demo-login", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			PrincipalID string `json:"principal_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("demo-login body: %v", err)
		}
		if request.PrincipalID != "principal_000001" {
			http.Error(w, `{"error":{"code":"unknown_principal"}}`, http.StatusBadRequest)
			return
		}
		f.loggedInAs = request.PrincipalID
		http.SetCookie(w, &http.Cookie{Name: "llm_bridge_principal_session", Value: "signed-" + request.PrincipalID, Path: "/", HttpOnly: true})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(serviceTokenHeader)
		cookie, _ := r.Cookie("llm_bridge_principal_session")
		if token == "" && cookie == nil {
			http.Error(w, `{"error":{"code":"not_logged_in"}}`, http.StatusUnauthorized)
			return
		}
		f.createdWithToken = token
		f.createdAsHeader = r.Header.Get(principalIDHeader)
		if cookie != nil {
			f.createdByCookie = cookie.Value
		}
		_, _ = w.Write([]byte(`{"session_id":"br_test"}`))
	})
	return mux
}

func TestAPrincipalWithNoServiceTokenLogsInAndCreatesTheSessionWithTheCookie(t *testing.T) {
	fake := &fakeGatedServer{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	d, err := authenticatedDelegate(context.Background(), srv.URL, "", "principal_000001")
	if err != nil {
		t.Fatalf("authenticatedDelegate: %v", err)
	}
	if _, err := d.createSession(context.Background(), "claude_code", "", "delegate", "hello", map[string]any{}); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if fake.loggedInAs != "principal_000001" {
		t.Errorf("logged in as %q, want principal_000001", fake.loggedInAs)
	}
	if fake.createdByCookie != "signed-principal_000001" {
		t.Errorf("create carried cookie %q, want the login's", fake.createdByCookie)
	}
	if fake.createdWithToken != "" || fake.createdAsHeader != "" {
		t.Errorf("a cookie caller sent service headers: token=%q principal=%q", fake.createdWithToken, fake.createdAsHeader)
	}
}

func TestAServiceTokenIsSentWithThePrincipalItActsAs(t *testing.T) {
	fake := &fakeGatedServer{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	d, err := authenticatedDelegate(context.Background(), srv.URL, "service-token-value", "principal_000001")
	if err != nil {
		t.Fatalf("authenticatedDelegate: %v", err)
	}
	if _, err := d.createSession(context.Background(), "claude_code", "", "delegate", "hello", map[string]any{}); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if fake.loggedInAs != "" {
		t.Errorf("a service-token caller logged in as %q; it must not", fake.loggedInAs)
	}
	if fake.createdWithToken != "service-token-value" || fake.createdAsHeader != "principal_000001" {
		t.Errorf("create carried token=%q principal=%q, want the token and principal_000001", fake.createdWithToken, fake.createdAsHeader)
	}
}

func TestNoCredentialStopsBeforeCallingTheServer(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	_, err := authenticatedDelegate(context.Background(), srv.URL, "", "")
	if err == nil || !strings.Contains(err.Error(), "--principal") || !strings.Contains(err.Error(), serviceTokenEnvironmentVariable) {
		t.Fatalf("err = %v, want one naming both --principal and %s", err, serviceTokenEnvironmentVariable)
	}
	if called {
		t.Error("the server was called with no credential")
	}
}

func TestARefusedLoginIsAnErrorNamingThePrincipal(t *testing.T) {
	srv := httptest.NewServer((&fakeGatedServer{}).handler(t))
	defer srv.Close()

	_, err := authenticatedDelegate(context.Background(), srv.URL, "", "principal_999999")
	if err == nil || !strings.Contains(err.Error(), "principal_999999") || !strings.Contains(err.Error(), "unknown_principal") {
		t.Fatalf("err = %v, want the refusal naming principal_999999", err)
	}
}
