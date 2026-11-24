package harness

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	logstore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/client"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// newManagerOnLogStoreURL is NewManager with the log-store HTTP client at
// logStoreURL as its session event log, for a test that serves a fake
// log-store. An empty URL leaves the manager with none.
func newManagerOnLogStoreURL(st *store.Store, logStoreURL, publicServerURL, localBridgeURL string, ptyRingBytes int, authClient *authstoreclient.Client) *Manager {
	m := NewManager(st, publicServerURL, localBridgeURL, ptyRingBytes, authClient)
	if logStoreURL != "" {
		m.SetSessionEventLog(logstore.New(logStoreURL))
	}
	return m
}

// A manager the server has not yet given a session event log refuses every
// read of it, and its writes are not retried: nothing will name a log under a
// running queue, so retrying would only hold the session's events for the
// retry deadline.
func TestAManagerWithNoSessionEventLogRefusesAndDoesNotRetry(t *testing.T) {
	m := NewManager(temporaryStore(t), "", "", 0, nil)
	if _, err := m.sessionEventLog().GetTurnState("br_test"); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("GetTurnState error = %v, want ErrSessionEventLogNotEnabled", err)
	}
	if _, err := m.SessionsHoldingHarnessSessionID("cc-uuid"); !errors.Is(err, ErrSessionEventLogNotEnabled) {
		t.Fatalf("SessionsHoldingHarnessSessionID error = %v, want ErrSessionEventLogNotEnabled", err)
	}
	_, err := m.sessionEventLog().PushEvent(msg.Event{BridgeSessionID: "br_test", Type: msg.EventUserMessage})
	if why := whyNotToRetryLogStorePush(err); why == "" {
		t.Fatalf("a push with no session event log would be retried: %v", err)
	}
}

func temporaryStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
