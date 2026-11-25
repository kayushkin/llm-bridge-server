package main

import (
	"fmt"
	"log"
	"net/url"

	timeledgerstore "github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore/internal/storeclient"
)

// tripTotal is one row of budget-store's GET /trips/totals.
type tripTotal struct {
	TripRef string `json:"trip_ref"`
}

// tripTransaction is one row of budget-store's GET /trips/{trip_ref}/transactions.
type tripTransaction struct {
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	DecidedBy     string `json:"decided_by"`
	OccurredOn    string `json:"occurred_on"`
	AmountCents   int64  `json:"amount_cents"`
}

// budget-store's statuses for a transaction linked to a trip.
const (
	tripTransactionConfirmed = "confirmed"
	tripTransactionSuggested = "suggested"
	tripTransactionRemoved   = "removed"
)

// spendingChanges counts what gatherTripSpending did.
type spendingChanges struct {
	tripsWithoutEntry, linked, takenOff int
}

// gatherTripSpending copies budget-store's trip spending onto the time entry
// each trip is the source of. budget-store decides which charges belong to a
// trip; this mirrors its answer and decides nothing. A confirmed charge
// bears on the entry as cost, a suggested one as context for the reviewer to
// weigh, and a removed one is taken off again if this program linked it. A
// link made by a model or the operator is left as it is.
func gatherTripSpending(ledgerURL, budgetStoreURL string) (spendingChanges, error) {
	changes := spendingChanges{}
	var trips []tripTotal
	if err := storeclient.Get(budgetStoreURL+"/trips/totals", &trips); err != nil {
		return changes, err
	}
	for _, trip := range trips {
		var sourceLinks []timeledgerstore.Link
		if err := storeclient.Get(ledgerURL+"/links/trips?"+url.Values{"ref": {trip.TripRef}}.Encode(), &sourceLinks); err != nil {
			return changes, err
		}
		entryID := ""
		for _, sourceLink := range sourceLinks {
			if sourceLink.RemovedAt == nil && sourceLink.BearsOn == timeledgerstore.BearsOnSource {
				entryID = sourceLink.TimeEntryID
			}
		}
		if entryID == "" {
			changes.tripsWithoutEntry++
			continue
		}
		var entry timeledgerstore.TimeEntry
		if err := storeclient.Get(ledgerURL+"/time-entries/"+entryID, &entry); err != nil {
			return changes, err
		}
		liveTransactionLinks := map[string]timeledgerstore.Link{}
		for _, existing := range entry.Links {
			if existing.LinkType == "transactions" && existing.RemovedAt == nil {
				liveTransactionLinks[existing.Ref] = existing
			}
		}
		var transactions []tripTransaction
		if err := storeclient.Get(budgetStoreURL+"/trips/"+url.PathEscape(trip.TripRef)+"/transactions", &transactions); err != nil {
			return changes, err
		}
		for _, charge := range transactions {
			wantedBearsOn, reason, err := tripChargeBearing(trip.TripRef, charge)
			if err != nil {
				return changes, err
			}
			existing, isLinked := liveTransactionLinks[charge.TransactionID]
			if isLinked && existing.BearsOn == wantedBearsOn && existing.AmountCents != nil && *existing.AmountCents == charge.AmountCents {
				continue
			}
			if isLinked {
				if existing.LinkedBy != signer {
					continue
				}
				if err := storeclient.Do("DELETE", ledgerURL+"/time-entries/"+entryID+"/links/transactions?"+url.Values{
					"ref": {charge.TransactionID}, "by": {signer}, "by_kind": {timeledgerstore.ActorProgram}}.Encode(), nil, nil); err != nil {
					return changes, err
				}
				changes.takenOff++
			}
			if wantedBearsOn == "" {
				continue
			}
			made, err := link(ledgerURL, entryID, map[string]any{"ref": charge.TransactionID, "amount_cents": charge.AmountCents,
				"bears_on": wantedBearsOn, "reason": reason}, "transactions")
			if err != nil {
				return changes, err
			}
			if made {
				changes.linked++
			}
		}
	}
	return changes, nil
}

// tripChargeBearing says how a charge budget-store keeps on a trip bears on
// the trip's entry, and why; an empty bearing means it should not be linked.
func tripChargeBearing(tripRef string, charge tripTransaction) (string, string, error) {
	switch charge.Status {
	case tripTransactionConfirmed:
		return timeledgerstore.BearsOnCost, fmt.Sprintf("budget-store has this %s charge confirmed for %s by %s", charge.OccurredOn, tripRef, charge.DecidedBy), nil
	case tripTransactionSuggested:
		return timeledgerstore.BearsOnContext, fmt.Sprintf("budget-store suggests this %s charge for %s, not yet confirmed: %s", charge.OccurredOn, tripRef, charge.Reason), nil
	case tripTransactionRemoved:
		return "", "", nil
	}
	return "", "", fmt.Errorf("budget-store gave %s on %s the status %q, which this program does not know", charge.TransactionID, tripRef, charge.Status)
}

func logSpendingChanges(changes spendingChanges) {
	log.Printf("trip spending: %d links added, %d taken off, %d trips with spending have no time entry yet",
		changes.linked, changes.takenOff, changes.tripsWithoutEntry)
}
