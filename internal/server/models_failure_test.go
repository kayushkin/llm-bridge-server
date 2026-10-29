package server

import (
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelsReportsAuthStoreFailureWithoutInventingEmptyCredentials(t *testing.T) {
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
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "auth_store_unavailable") || calls != 1 {
		t.Fatalf("status %d, calls %d, body %s", response.StatusCode, calls, body)
	}
}
