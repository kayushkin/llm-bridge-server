package mailstack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func createIngressCredential(t *testing.T, s *Server, accounts string) (string, string) {
	t.Helper()
	w := serve(t, s, http.MethodPost, "/api/ingress/credentials",
		fmt.Sprintf(`{"source":"databricks","note":"raw-mail push","account_ids":%s}`, accounts), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create credential %d: %s", w.Code, w.Body)
	}
	var got struct {
		Credential struct {
			ID string `json:"id"`
		} `json:"credential"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Token == "" {
		t.Fatalf("decode credential %s: %v", w.Body, err)
	}
	return got.Credential.ID, got.Token
}

func pushedDelivery(deliveryID, account, sourceRevision, body string) string {
	return fmt.Sprintf(`{"delivery_id":%q,"account_id":%q,"message":{"provider_message_id":"AAMk-push","source_revision":%q,
		"message_id":"<push@outlook.example>","subject":"Lieferung","from":{"email":"kunde@customer.example"},
		"received_at":"2026-10-19T08:00:00Z","body_text":%q}}`, deliveryID, account, sourceRevision, body)
}

type deliverResponse struct {
	Results []struct {
		Status  string `json:"status"`
		Locator string `json:"locator"`
		Detail  string `json:"detail"`
		Retry   bool   `json:"retry"`
	} `json:"results"`
	Counts map[string]int `json:"counts"`
}

func deliver(t *testing.T, s *Server, token string, deliveries ...string) (int, deliverResponse) {
	t.Helper()
	header := map[string]string{}
	if token != "" {
		header[IngressTokenHeader] = token
	}
	w := serve(t, s, http.MethodPost, "/api/ingress/deliveries", `{"deliveries":[`+strings.Join(deliveries, ",")+`]}`, header)
	var got deliverResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", w.Body, err)
		}
	}
	return w.Code, got
}

func TestPushedDeliveriesAreAcknowledgedOneByOne(t *testing.T) {
	s := localTestServer(t)
	_, token := createIngressCredential(t, s, `["demo-local"]`)

	if code, _ := deliver(t, s, "", pushedDelivery("d1", "demo-local", "ck-1", "x")); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code, _ := deliver(t, s, "msi_wrong", pushedDelivery("d1", "demo-local", "ck-1", "x")); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", code)
	}

	code, got := deliver(t, s, token,
		pushedDelivery("d1", "demo-local", "ck-1", "hallo"),
		pushedDelivery("d2", "demo-local", "ck-1", "hallo"),
		pushedDelivery("d3", "demo-local", "ck-1", "something else"),
		pushedDelivery("d4", "acct-ms", "ck-1", "hallo"),
		pushedDelivery("d5", "demo-local", "", "no revision"),
	)
	if code != http.StatusOK {
		t.Fatalf("deliver: %d", code)
	}
	want := []string{"accepted", "replay", "conflict", "rejected", "rejected"}
	for i, status := range want {
		if got.Results[i].Status != status {
			t.Fatalf("delivery %d: %s (%s), want %s", i, got.Results[i].Status, got.Results[i].Detail, status)
		}
		if got.Results[i].Retry {
			t.Fatalf("delivery %d says retry; only a failure should", i)
		}
	}

	// The accepted message reads like any other, and is announced once.
	w := serve(t, s, http.MethodGet, "/api/messages/"+strings.TrimPrefix(got.Results[0].Locator, "demo-local:")+"/body?account=demo-local&format=text", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "hallo" {
		t.Fatalf("read the delivered message: %d %q", w.Code, w.Body)
	}
	w = serve(t, s, http.MethodGet, "/api/events?after=0", "", nil)
	var events struct {
		Events []struct {
			Kind, Origin, Locator string
		} `json:"events"`
		NextAfter int64 `json:"next_after"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil || len(events.Events) != 1 || events.Events[0].Origin != "ingress" || events.Events[0].Locator != got.Results[0].Locator {
		t.Fatalf("events %s", w.Body)
	}
	if w := serve(t, s, http.MethodGet, fmt.Sprintf("/api/events?after=%d", events.NextAfter), "", nil); !strings.Contains(w.Body.String(), `"events": []`) {
		t.Fatalf("after the cursor: %s", w.Body)
	}

	w = serve(t, s, http.MethodGet, "/api/ingress/receipts?status=conflict", "", nil)
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), `"receipt_id"`) != 1 {
		t.Fatalf("conflict receipts %d: %s", w.Code, w.Body)
	}
}

func TestACredentialDeliversOnlyIntoItsAccounts(t *testing.T) {
	s := localTestServer(t)
	id, token := createIngressCredential(t, s, `["demo-local"]`)
	if w := serve(t, s, http.MethodPost, "/api/ingress/credentials", `{"source":"databricks","account_ids":["acct-ms"]}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("binding a non-local account: %d, want 400", w.Code)
	}
	w := serve(t, s, http.MethodGet, "/api/ingress/credentials", "", nil)
	if strings.Contains(w.Body.String(), token) || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("credential list must name the credential and never its token: %s", w.Body)
	}
	// An ingress token is not the service bearer and opens nothing else.
	if w := serve(t, s, http.MethodPost, "/api/ingress/credentials/"+id+"/revoke", "", nil); w.Code != http.StatusOK {
		t.Fatalf("revoke %d", w.Code)
	}
	if code, _ := deliver(t, s, token, pushedDelivery("d1", "demo-local", "ck-1", "x")); code != http.StatusUnauthorized {
		t.Fatalf("revoked credential: %d, want 401", code)
	}
}

func TestPurgeLeavesATombstoneEveryWayInHonours(t *testing.T) {
	s := localTestServer(t)
	_, token := createIngressCredential(t, s, `["demo-local"]`)
	_, got := deliver(t, s, token, pushedDelivery("d1", "demo-local", "ck-1", "hallo"))
	id := strings.TrimPrefix(got.Results[0].Locator, "demo-local:")

	if w := serve(t, s, http.MethodPost, "/api/messages/"+id+"/purge?account=demo-local", `{}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("purge without a reason: %d, want 400", w.Code)
	}
	if w := serve(t, s, http.MethodPost, "/api/messages/"+id+"/purge?account=demo-local", `{"reason":"retention"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("purge %d: %s", w.Code, w.Body)
	}
	_, again := deliver(t, s, token, pushedDelivery("d2", "demo-local", "ck-2", "back"))
	if again.Results[0].Status != "purged" {
		t.Fatalf("delivery after purge: %+v", again.Results[0])
	}
	importBody := `{"messages":[{"provider_message_id":"AAMk-push","received_at":"2026-10-19T08:00:00Z","body_text":"back"}]}`
	if w := serve(t, s, http.MethodPost, "/api/accounts/demo-local/messages/import", importBody, nil); w.Code != http.StatusConflict {
		t.Fatalf("import after purge: %d, want 409: %s", w.Code, w.Body)
	}
	w := serve(t, s, http.MethodGet, "/api/events", "", nil)
	if !strings.Contains(w.Body.String(), `"message.purged"`) {
		t.Fatalf("no purge event: %s", w.Body)
	}
}

func TestThePushSurfaceIsForServices(t *testing.T) {
	s, _, _ := accessTestServer(t, true)
	for _, path := range []string{"/api/events", "/api/ingress/receipts", "/api/ingress/credentials"} {
		if w := serve(t, s, http.MethodGet, path, "", as("principal_000001")); w.Code != http.StatusForbidden {
			t.Errorf("a person on %s: %d, want 403", path, w.Code)
		}
	}
}
