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
	"errors"
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
	if !scope.IsEveryOwner() {
		// The caller's own books, or the one owner an administrator named:
		// a record kept by anyone else is not found there.
		return scope.OwnerPrincipalID(), true
	}
	id := r.PathValue(ownerOf.pathValue)
	for _, lookup := range ownerOf.lookups {
		owner, err := store.ownerOfRecord(r.Context(), lookup.table, lookup.column, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			writeError(w, err)
			return "", false
		}
		return owner, true
	}
	writeError(w, ErrNotFound)
	return "", false
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
