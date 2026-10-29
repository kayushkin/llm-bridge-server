package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestExternalDiscoveryRetainsConfiguredOwnerAndRejectsDisabledOwner(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "disabled"}[disabled], func(t *testing.T) {
			srv, st := testServer(t)
			owner := principalclient.Principal{ID: "local-owner", Kind: "human"}
			if disabled {
				owner.DisabledAt = 1
			}
			directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/principals/local-owner" {
					t.Errorf("unexpected owner lookup: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(owner); err != nil {
					t.Error(err)
				}
			}))
			defer directory.Close()
			srv.cfg.DiscoveryPrincipalID = owner.ID
			srv.principalClient = principalclient.New(directory.URL)
			now := time.Now().UTC()
			srv.recordExternalSession(context.Background(), msg.StoredSession{Harness: msg.HarnessCodex, HarnessSessionID: "external-owner-fixture", Source: msg.PurposeChat, CreatedAt: now, UpdatedAt: now}, "")
			row, err := st.GetSessionByHarnessSessionID("external-owner-fixture")
			if disabled {
				if err == nil && row != nil {
					t.Fatal("recorded a session for a disabled configured owner")
				}
				return
			}
			if err != nil || row == nil {
				t.Fatalf("external session was not recorded: %v", err)
			}
			if row.PrincipalID != owner.ID {
				t.Fatalf("owner = %q, want %q", row.PrincipalID, owner.ID)
			}
		})
	}
}
