package inprocesshttp

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTheHandlersAnswerArrivesAsItWroteIt(t *testing.T) {
	client := Client(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files/file_000001" || r.URL.Query().Get("inline") != "true" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusCreated)
		w.Header().Set("X-Too-Late", "yes")
		w.Write([]byte("got " + string(body)))
	}))
	response, err := client.Post(BaseURL+"/files/file_000001?inline=true", "text/plain", strings.NewReader("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated || string(body) != "got bytes" || response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("X-Too-Late") != "" {
		t.Fatalf("%d %q %v", response.StatusCode, body, response.Header)
	}
	missing, err := client.Get(BaseURL + "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("an unrouted path: %d", missing.StatusCode)
	}
}

func TestAHandlerThatWritesNothingAnswersOK(t *testing.T) {
	response, err := Client(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Get(BaseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d", response.StatusCode)
	}
}
