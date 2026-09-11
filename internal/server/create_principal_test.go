package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakePrincipalStore knows one principal and answers principal-store's own
// {"error":…} 404 for any other.
func fakePrincipalStore(t *testing.T, known string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /principals/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") != known {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found: principal ` + r.PathValue("id") + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + known + `","kind":"human","display_name":"Vlad","disabled_at":0,"groups":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func principalRequest(instanceID, principalID string) msg.CreateSessionRequest {
	return msg.CreateSessionRequest{
		Type: msg.SessionTypeInteractive, Purpose: msg.PurposeChat, Origin: "test",
		Harness: msg.HarnessMock, InstanceID: instanceID, PrincipalID: principalID,
	}
}

func TestCreateSessionStoresAKnownPrincipalAndRefusesAnUnknownOne(t *testing.T) {
	srv, st, instanceID := testServerWithInstance(t, msg.HarnessMock)
	directory := fakePrincipalStore(t, "principal_000001")
	srv.principalClient = principalclient.New(directory.URL)
	// No grants at all: the gate lets everything through, which is the case
	// this test is about — the principal check itself.
	srv.grantClient = grantclient.New(fakeGrantStoreByRelation(t, "principal_000001", map[string][]string{}).URL)

	created := postCreateSession(t, srv, principalRequest(instanceID, "principal_000001"))
	if created.PrincipalID != "principal_000001" {
		t.Fatalf("created.PrincipalID = %q, want principal_000001", created.PrincipalID)
	}
	stored, err := st.GetSession(created.SessionID)
	if err != nil || stored.PrincipalID != "principal_000001" {
		t.Fatalf("stored principal = %q (%v), want principal_000001 — the column round-trips", stored.PrincipalID, err)
	}

	for _, c := range []struct {
		principal string
		status    int
		code      string
	}{
		{"principal_000404", 400, "unknown_principal"},
		{"Vlad Kayushkin", 400, "invalid_principal_id"},
	} {
		resp := doJSON(t, srv, "POST", "/sessions", principalRequest(instanceID, c.principal))
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || !strings.Contains(string(raw), c.code) {
			t.Fatalf("%q: status = %d body = %s, want %d %s", c.principal, resp.StatusCode, raw, c.status, c.code)
		}
	}

	// A directory that is down is not the caller's fault and not a 400.
	directory.Close()
	resp := doJSON(t, srv, "POST", "/sessions", principalRequest(instanceID, "principal_000001"))
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || !strings.Contains(string(raw), "principal_store_unavailable") {
		t.Fatalf("directory down: status = %d body = %s, want 502 principal_store_unavailable", resp.StatusCode, raw)
	}

	// No principal: created as before, nothing asked.
	none := postCreateSession(t, srv, principalRequest(instanceID, ""))
	if none.PrincipalID != "" {
		t.Fatalf("a session with no principal stored %q", none.PrincipalID)
	}
}
