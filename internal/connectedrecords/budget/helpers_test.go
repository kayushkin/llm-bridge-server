package budget

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// The owners of the test books. Made up.
const (
	testOwnerPrincipalID  = "principal_000101"
	otherOwnerPrincipalID = "principal_000102"
	administratorID       = "principal_000103"
)

// testStores remembers the Store each test ledger came from, for the tests
// that also serve routes.
var testStores sync.Map

// newTestStore is a store on a database of the test's own.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	store := New(database.Open(module))
	store.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return store
}

// openLedger opens owner's books in store for the rest of the test.
func openLedger(t *testing.T, store *Store, owner string) *Ledger {
	t.Helper()
	ledger, err := store.OpenLedger(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Error(err)
		}
	})
	testStores.Store(ledger, store)
	return ledger
}

// openTestStore is the test owner's books on a database of the test's own.
func openTestStore(t *testing.T) *Ledger {
	t.Helper()
	return openLedger(t, newTestStore(t), testOwnerPrincipalID)
}

// testStoreOf is the store a test ledger came from.
func testStoreOf(t *testing.T, ledger *Ledger) *Store {
	t.Helper()
	store, found := testStores.Load(ledger)
	if !found {
		t.Fatal("the ledger was not opened by openLedger")
	}
	return store.(*Store)
}

// asCaller is r as the server hands it on for principal.
func asCaller(r *http.Request, principal string) *http.Request {
	return r.WithContext(ownership.ContextWithCaller(r.Context(), ownership.Caller{PrincipalID: principal}))
}

// asAdministrator is r as the server hands it on for an administrator.
func asAdministrator(r *http.Request) *http.Request {
	return r.WithContext(ownership.ContextWithCaller(r.Context(), ownership.Caller{PrincipalID: administratorID, SeesEveryOwner: true}))
}
