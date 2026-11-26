package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

// An auth-store failure is answered as JSON with a code, whatever the error
// text holds: it used to be pasted into a JSON string by hand, so a quote in
// it broke the body.
func TestCredentialsListAnswersAnAuthStoreFailureAsJSONWithACode(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `the "credentials" table is locked`, http.StatusServiceUnavailable)
	}))
	defer auth.Close()
	server, _ := testServer(t)
	server.authClient = authstoreclient.New(auth.URL, "", "")
	response := doJSON(t, server, "GET", "/credentials", nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var answer struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if response.StatusCode != http.StatusBadGateway || json.Unmarshal(body, &answer) != nil || answer.Error.Code != "auth_store_unavailable" || answer.Error.Message == "" {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type %q", got)
	}
}
