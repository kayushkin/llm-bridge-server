package harness

import (
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	sessionlogclient "github.com/kayushkin/llm-bridge-server/internal/sessionlog/client"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/store/storetest"
	"github.com/kayushkin/llm-bridge/msg"
)

// newManagerOnLogStoreURL is NewManager with the log-store HTTP client at
// logStoreURL as its session event log, for a test that serves a fake
// log-store. An empty URL leaves the manager with none.
func newManagerOnLogStoreURL(st *store.Store, logStoreURL, publicServerURL, localBridgeURL string, ptyRingBytes int, authClient *authstoreclient.Client) *Manager {
	m := NewManager(st, publicServerURL, localBridgeURL, ptyRingBytes, authClient)
	if logStoreURL != "" {
		m.SetSessionEventLog(sessionlogclient.New(logStoreURL))
	}
	return m
}

// A manager the server has not yet given a session event log or event store
// refuses every write and read of either.
func TestAManagerWithNoSessionEventLogRefuses(t *testing.T) {
	m := NewManager(temporaryStore(t), "", "", 0, nil)
	if _, err := m.sessionEventLog().GetTurnState("br_test"); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("GetTurnState error = %v, want ErrSessionEventLogNotEnabled", err)
	}
	if _, err := m.SessionsHoldingHarnessSessionID("cc-uuid"); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("SessionsHoldingHarnessSessionID error = %v, want ErrSessionEventLogNotEnabled", err)
	}
	if _, err := m.sessionEventLog().PushEvent(msg.Event{BridgeSessionID: "br_test", Type: msg.EventUserMessage}); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("PushEvent error = %v, want ErrSessionEventLogNotEnabled", err)
	}
	if _, err := m.SessionEvents().StoreSessionEvent(store.SessionEventRecord{SessionID: "br_test", Type: string(msg.EventUserMessage)}); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("StoreSessionEvent error = %v, want ErrSessionEventLogNotEnabled", err)
	}
	if _, err := m.SessionEvents().MaxEventRowID("br_test"); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("MaxEventRowID error = %v, want ErrSessionEventLogNotEnabled", err)
	}
}

func temporaryStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.New(t)
}
