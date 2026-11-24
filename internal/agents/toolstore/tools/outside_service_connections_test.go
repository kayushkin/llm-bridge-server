package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Browser, WebSearch and Scheduler read no environment variable: each is built
// with where its outside service is. These tests set the variables the tools
// used to read to a value that would be wrong, and assert the argument won.

// requestsSeenBy starts a stub service and records the Authorization header of
// each request it answers.
func requestsSeenBy(t *testing.T, body string) (baseURL string, authorizations *[]string) {
	t.Helper()
	seen := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL, &seen
}

func TestTheSchedulerToolCallsTheConnectionItWasBuiltWith(t *testing.T) {
	t.Setenv("SCHEDULER_URL", "http://127.0.0.1:1")
	t.Setenv("SCHEDULER_TOKEN", "token-from-the-environment")
	baseURL, authorizations := requestsSeenBy(t, `[]`)

	if _, err := Scheduler(SchedulerConnection{BaseURL: baseURL, Token: "token-from-the-argument"}).Run(context.Background(), `{"action":"list"}`); err != nil {
		t.Fatal(err)
	}
	if len(*authorizations) != 1 || (*authorizations)[0] != "Bearer token-from-the-argument" {
		t.Errorf("the stub scheduler saw Authorization headers %q, want one request carrying the argument's token", *authorizations)
	}

	if _, err := Scheduler(SchedulerConnection{BaseURL: baseURL}).Run(context.Background(), `{"action":"list"}`); err != nil {
		t.Fatal(err)
	}
	if len(*authorizations) != 2 || (*authorizations)[1] != "" {
		t.Errorf("with no token the stub scheduler saw %q, want a second request with no Authorization header", *authorizations)
	}
}

func TestTheBrowserToolCallsTheConnectionItWasBuiltWith(t *testing.T) {
	t.Setenv("PINCHTAB_URL", "http://127.0.0.1:1")
	t.Setenv("PINCHTAB_TOKEN", "token-from-the-environment")
	baseURL, authorizations := requestsSeenBy(t, `page text`)

	// A trailing slash on the base URL is trimmed, as it was when the URL came
	// from PINCHTAB_URL.
	output, err := Browser(PinchtabConnection{BaseURL: baseURL + "/", Token: "token-from-the-argument"}).Run(context.Background(), `{"action":"text"}`)
	if err != nil {
		t.Fatal(err)
	}
	if output != "page text" {
		t.Errorf("the browser tool answered %q, want the stub's page text", output)
	}
	if len(*authorizations) != 1 || (*authorizations)[0] != "Bearer token-from-the-argument" {
		t.Errorf("the stub PinchTab saw Authorization headers %q, want one request carrying the argument's token", *authorizations)
	}
}

func TestWebSearchWithNoKeySaysSoWhateverTheEnvironmentHolds(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "key-from-the-environment")
	output, err := WebSearch("").Run(context.Background(), `{"query":"anything"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "BRAVE_API_KEY") || !strings.HasPrefix(output, "error:") {
		t.Errorf("web_search with no key answered %q, want the error naming BRAVE_API_KEY", output)
	}
}

// The three tools are in the registry only once a program has said where their
// services are, and then under the names the seeded rows carry.
func TestTheOutsideServiceToolsAreRegisteredOnlyWhenAsked(t *testing.T) {
	names := []string{"browser", "web_search", "scheduler"}
	for _, name := range names {
		if _, registered := ByName(name); registered {
			t.Fatalf("%s is registered before anyone supplied its connection", name)
		}
	}
	RegisterToolsThatReachOutsideServices(OutsideServiceConnections{})
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		for _, name := range names {
			delete(registry, name)
		}
	})
	for _, name := range names {
		if _, registered := ByName(name); !registered {
			t.Errorf("%s is not registered after RegisterToolsThatReachOutsideServices", name)
		}
	}
}
