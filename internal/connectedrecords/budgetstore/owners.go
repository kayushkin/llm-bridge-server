package budgetstore

// Whose books a request reads and writes.
//
// A request reaches the store with an ownership.Caller in its context, which
// the server set from the credential it verified, never from anything the
// client sent. Every route works in one owner's books, since a balance, a
// budget, a report or a matching pass only means something inside one
// person's ledger:
//
//   - A route that names one of the store's records by id (an account, a
//     transaction, a category…) works in the books of the owner who keeps it.
//     A caller who does not see every owner reaches only their own records;
//     another owner's record answers 404, as a missing one does. An
//     administrator, or this server's own service acting as itself, reaches
//     any owner's record by its id, or names the owner in owner_principal_id.
//   - Every other route (lists, reports, new records, passes) works in the
//     books ownership.OwnerActedFor names: the caller's own, or, for a caller
//     who sees every owner, the books owner_principal_id names. The service
//     acting as nobody must name one.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// recordOwner says how a route finds the books it works in: from the record
// its path names (each table and column tried in turn), or, with no lookups,
// from ownership.OwnerActedFor.
type recordOwner struct {
	pathValue string
	lookups   []ownerLookup
}

// ownerLookup is a table and the column of it that holds a path's id.
type ownerLookup struct{ table, column string }

func ownedRecord(pathValue string, lookups ...ownerLookup) recordOwner {
	return recordOwner{pathValue: pathValue, lookups: lookups}
}

var (
	actedFor         = recordOwner{}
	accountOwner     = ownedRecord("id", ownerLookup{"accounts", "id"})
	categoryOwner    = ownedRecord("id", ownerLookup{"categories", "id"})
	payeeOwner       = ownedRecord("id", ownerLookup{"payees", "id"})
	importBatchOwner = ownedRecord("id", ownerLookup{"import_batches", "id"})
	// A transaction the transfer matcher folded into another still answers
	// by its own id, so its owner is found among the merges too.
	transactionOwner          = ownedRecord("id", ownerLookup{"transactions", "id"}, ownerLookup{"transaction_merges", "merged_transaction_id"})
	recurringBillOwner        = ownedRecord("id", ownerLookup{"recurring_bills", "id"})
	marketplaceOrderOwner     = ownedRecord("id", ownerLookup{"marketplace_orders", "id"})
	marketplaceOrderLineOwner = ownedRecord("id", ownerLookup{"marketplace_order_lines", "id"})
	interestOwner             = ownedRecord("id", ownerLookup{"interests", "id"})
	plaidAccountOwner         = ownedRecord("plaid_account_id", ownerLookup{"plaid_accounts", "plaid_account_id"})
)

// respondRefusal writes refusal, and reports whether there was one.
func respondRefusal(w http.ResponseWriter, refusal *ownership.RequestError) bool {
	if refusal == nil {
		return false
	}
	writeJSON(w, refusal.Status, map[string]string{"error": refusal.Message})
	return true
}

// ownerOfRequest is whose books the request works in.
func (store *Store) ownerOfRequest(w http.ResponseWriter, r *http.Request, ownerOf recordOwner) (string, bool) {
	if len(ownerOf.lookups) == 0 {
		owner, refusal := ownership.OwnerActedFor(r)
		return owner, !respondRefusal(w, refusal)
	}
	scope, refusal := ownership.ReadScope(r)
	if respondRefusal(w, refusal) {
		return "", false
	}
	owner, err := store.ownerOfRecordInScope(r.Context(), scope, ownerOf, r.PathValue(ownerOf.pathValue))
	if err != nil {
		writeError(w, err)
		return "", false
	}
	return owner, true
}

// ownerOfRecordInScope is whose books hold the record id names, for a read
// in scope: the one owner scope names, whose books a record kept by anyone
// else is not found in, or, for every owner's scope, the owner who keeps it.
func (store *Store) ownerOfRecordInScope(ctx context.Context, scope ownership.Scope, ownerOf recordOwner, id string) (string, error) {
	if !scope.IsEveryOwner() {
		return scope.OwnerPrincipalID(), nil
	}
	for _, lookup := range ownerOf.lookups {
		owner, err := store.ownerOfRecord(ctx, lookup.table, lookup.column, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		return owner, nil
	}
	return "", ErrNotFound
}

// RecordKind is a kind of record the store answers by id alone, by the
// path its GET route is under: "accounts" for GET /accounts/{id}.
type RecordKind string

// The kinds a reference can name.
const (
	RecordKindAccount              RecordKind = "accounts"
	RecordKindTransaction          RecordKind = "transactions"
	RecordKindCategory             RecordKind = "categories"
	RecordKindPayee                RecordKind = "payees"
	RecordKindMarketplaceOrder     RecordKind = "marketplace-orders"
	RecordKindMarketplaceOrderLine RecordKind = "marketplace-order-lines"
	RecordKindImportBatch          RecordKind = "import-batches"
)

// recordByID is how one kind's record is found: whose books keep it, and
// the ledger's read of it there.
type recordByID struct {
	owner recordOwner
	read  func(ledger *Ledger, id string) (any, error)
}

func readAs[T any](read func(*Ledger, string) (T, error)) func(*Ledger, string) (any, error) {
	return func(ledger *Ledger, id string) (any, error) { return read(ledger, id) }
}

// recordsByID is every kind GET /<kind>/{id} answers, the one table the
// routes and RecordInScope read.
var recordsByID = map[RecordKind]recordByID{
	RecordKindAccount:              {accountOwner, readAs((*Ledger).GetAccount)},
	RecordKindTransaction:          {transactionOwner, readAs((*Ledger).GetTransaction)},
	RecordKindCategory:             {categoryOwner, readAs((*Ledger).GetCategory)},
	RecordKindPayee:                {payeeOwner, readAs((*Ledger).GetPayee)},
	RecordKindMarketplaceOrder:     {marketplaceOrderOwner, readAs((*Ledger).GetMarketplaceOrder)},
	RecordKindMarketplaceOrderLine: {marketplaceOrderLineOwner, readAs((*Ledger).GetMarketplaceOrderLine)},
	RecordKindImportBatch:          {importBatchOwner, readAs((*Ledger).GetImportBatch)},
}

// recordRoute is GET /<kind>/{id}: the record, in its keeper's books.
func (store *Store) recordRoute(kind RecordKind) http.HandlerFunc {
	byID := recordsByID[kind]
	return store.ledgerRoute(byID.owner, func(ledger *Ledger, w http.ResponseWriter, r *http.Request) {
		record, err := byID.read(ledger, r.PathValue(byID.owner.pathValue))
		respond(w, http.StatusOK, record, err)
	})
}

// RecordInScope reads one record of kind by its id, in the books of the
// owner who keeps it, as GET /<kind>/{id} answers it to a caller reading in
// scope: a record outside scope is ErrNotFound, as a missing one is.
func (store *Store) RecordInScope(ctx context.Context, scope ownership.Scope, kind RecordKind, id string) (any, error) {
	byID, known := recordsByID[kind]
	if !known {
		return nil, fmt.Errorf("budget-store answers no record of kind %q by id", kind)
	}
	owner, err := store.ownerOfRecordInScope(ctx, scope, byID.owner, id)
	if err != nil {
		return nil, err
	}
	ledger, err := store.OpenLedger(ctx, owner)
	if err != nil {
		return nil, err
	}
	record, readErr := byID.read(ledger, id)
	if err := ledger.Close(); err != nil {
		log.Printf("budget-store: %v", err)
	}
	return record, readErr
}

// ledgerRoute serves one route in the books the request works in.
func (store *Store) ledgerRoute(ownerOf recordOwner, serve func(s *Ledger, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner, ok := store.ownerOfRequest(w, r, ownerOf)
		if !ok {
			return
		}
		ledger, err := store.OpenLedger(r.Context(), owner)
		if err != nil {
			writeError(w, err)
			return
		}
		defer func() {
			if err := ledger.Close(); err != nil {
				log.Printf("budget-store: %v", err)
			}
		}()
		serve(ledger, w, r)
	}
}
