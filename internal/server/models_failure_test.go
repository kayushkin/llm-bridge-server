package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

// An auth-store that cannot be read leaves the model list standing, because
// the chat's model picker reads it, with every row's credentials marked
// unknown rather than invented as an empty list.
func TestModelsAnswersWithCredentialsUnknownWhenAuthStoreFails(t *testing.T) {
	calls := 0
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "credential service unavailable", http.StatusServiceUnavailable)
	}))
	defer auth.Close()
	server, _ := testServer(t)
	server.authClient = authstoreclient.New(auth.URL, "", "")
	response := doJSON(t, server, "GET", "/models", nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("status %d, calls %d, body %s", response.StatusCode, calls, body)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(rows) == 0 {
		t.Fatalf("no models answered: %s", body)
	}
	for _, row := range rows {
		if string(row["credentials"]) != "null" || string(row["credentials_known"]) != "false" ||
			!strings.Contains(string(row["credentials_error"]), "503") {
			t.Fatalf("a row does not say its credentials are unknown: %s", body)
		}
	}
}

// When auth-store answers, a model with no credentials says so with an empty
// list, marked known.
func TestModelsMarksCredentialsKnownWhenAuthStoreAnswers(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer auth.Close()
	server, _ := testServer(t)
	server.authClient = authstoreclient.New(auth.URL, "", "")
	response := doJSON(t, server, "GET", "/models", nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
		t.Fatalf("status %d, body %s, err %v", response.StatusCode, body, err)
	}
	for _, row := range rows {
		if string(row["credentials"]) != "[]" || string(row["credentials_known"]) != "true" || row["credentials_error"] != nil {
			t.Fatalf("a row with auth-store answering: %s", body)
		}
	}
}
