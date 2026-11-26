package harness

import (
	"context"
	"sync"
	"testing"

	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/store/storetest"
)

// singleCopyTestStores is what a test manager runs on: the bridge core store
// and the session log, in one Postgres database of the test's own, with the
// session log the only copy of events, as the server wires them.
type singleCopyTestStores struct {
	database   *storetest.Database
	bridge     *store.Store
	sessionLog *logstoreserver.Server
	singleCopy *sessionevents.SingleCopy
}

func newSingleCopyTestStores(t testing.TB) *singleCopyTestStores {
	t.Helper()
	database := storetest.NewDatabase(t)
	bridge := database.OpenStore()
	sessionLog := database.OpenSessionLog(false)
	singleCopy, err := sessionevents.OpenSingleCopy(context.Background(), bridge, sessionLog)
	if err != nil {
		t.Fatalf("open the single copy: %v", err)
	}
	stores := &singleCopyTestStores{database: database, bridge: bridge, sessionLog: sessionLog, singleCopy: singleCopy}
	singleCopyStoresByBridgeStore.Store(bridge, stores)
	t.Cleanup(func() { singleCopyStoresByBridgeStore.Delete(bridge) })
	return stores
}

// singleCopyStoresByBridgeStore finds the stores a bridge store was opened
// with, for a test that keeps only the bridge store and builds a manager over
// it again, as a restarted server would.
var singleCopyStoresByBridgeStore sync.Map

func singleCopyStoresOf(t testing.TB, bridge *store.Store) *singleCopyTestStores {
	t.Helper()
	stores, ok := singleCopyStoresByBridgeStore.Load(bridge)
	if !ok {
		t.Fatal("this bridge store was not opened by newSingleCopyTestStores")
	}
	return stores.(*singleCopyTestStores)
}

// attach makes the stores m's session event log and event store.
func (stores *singleCopyTestStores) attach(m *Manager) *Manager {
	m.SetSessionEventLog(stores.sessionLog)
	m.SetSessionEventStore(stores.singleCopy)
	return m
}

// newSingleCopyTestManager is a manager on fresh single-copy stores.
func newSingleCopyTestManager(t testing.TB) (*Manager, *store.Store, *singleCopyTestStores) {
	t.Helper()
	stores := newSingleCopyTestStores(t)
	m := stores.attach(NewManager(stores.bridge, "http://127.0.0.1:0", "", 0, nil))
	return m, stores.bridge, stores
}
