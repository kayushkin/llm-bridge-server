package budgetstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type fakeVault struct {
	baseURL string
	saved   map[string]string
}

func (v *fakeVault) PlaidKeys(context.Context) (PlaidKeys, error) {
	return PlaidKeys{ClientID: "client", Secret: "secret", BaseURL: v.baseURL}, nil
}
func (v *fakeVault) SaveAccessToken(_ context.Context, itemID, _, token string) (string, error) {
	v.saved["cred_"+itemID] = token
	return "cred_" + itemID, nil
}
func (v *fakeVault) AccessToken(_ context.Context, id string) (string, error) {
	token, found := v.saved[id]
	if !found {
		return "", errors.New("no such credential")
	}
	return token, nil
}
func (v *fakeVault) Environment() string { return "sandbox" }

// fakePlaid answers the four Plaid routes the store calls. Each sync hands
// out the next page of changes from pages.
type fakePlaid struct {
	mu            sync.Mutex
	pages         [][]map[string]any // each: {added, modified, removed}
	interruptOnce bool
	syncCalls     int
}

func (f *fakePlaid) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body["client_id"] != "client" || body["secret"] != "secret" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error_code": "INVALID_API_KEYS", "error_message": "bad keys"})
		return
	}
	switch r.URL.Path {
	case "/item/public_token/exchange":
		json.NewEncoder(w).Encode(map[string]string{"access_token": "access-1", "item_id": "item-1"})
	case "/accounts/get":
		json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{
			{"account_id": "plaid-checking", "name": "CHECKING", "mask": "1111", "type": "depository", "subtype": "checking"},
			{"account_id": "plaid-savings", "name": "SAVINGS", "mask": "9999", "type": "depository", "subtype": "savings"},
		}})
	case "/transactions/sync":
		f.syncCalls++
		if f.interruptOnce && body["cursor"] != "" {
			f.interruptOnce = false
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error_code": "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION", "error_message": "restart"})
			return
		}
		cursor, _ := body["cursor"].(string)
		index := 0
		if cursor != "" {
			json.Unmarshal([]byte(cursor), &index)
		}
		if index >= len(f.pages) {
			json.NewEncoder(w).Encode(map[string]any{"added": []any{}, "modified": []any{}, "removed": []any{}, "next_cursor": cursor, "has_more": false})
			return
		}
		page := f.pages[index]
		next, _ := json.Marshal(index + 1)
		json.NewEncoder(w).Encode(map[string]any{"added": page[0]["added"], "modified": page[0]["modified"], "removed": page[0]["removed"],
			"next_cursor": string(next), "has_more": false})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func plaidTxn(id, pendingID, account string, amount float64, date, name string, pending bool) map[string]any {
	return map[string]any{"transaction_id": id, "pending_transaction_id": pendingID, "account_id": account, "amount": amount,
		"date": date, "name": name, "pending": pending}
}

func TestPlaidLinksABankAndSyncsWithoutCountingAnythingTwice(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking", LastFour: "1111"})
	// A downloaded file already brought the history up to Nov 3.
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+`"11/03/2026","GROCERY","-50.00","","Posted"`+"\n")
	plaid := &fakePlaid{interruptOnce: true}
	server := httptest.NewServer(http.HandlerFunc(plaid.serve))
	defer server.Close()
	if _, err := s.SyncPlaid(context.Background()); !errors.Is(err, ErrPlaidOff) {
		t.Fatalf("sync with no vault = %v, want plaid off", err)
	}
	s.SetPlaidVault(&fakeVault{baseURL: server.URL, saved: map[string]string{}})

	item, err := s.LinkPlaidItem(context.Background(), "public-1", "ins_1", "Test Bank")
	if err != nil {
		t.Fatal(err)
	}
	accounts := map[string]PlaidAccount{}
	for _, a := range item.Accounts {
		accounts[a.PlaidAccountID] = a
	}
	if accounts["plaid-checking"].AccountID != checking.ID || accounts["plaid-checking"].SyncFromOn != "2026-11-04" {
		t.Fatalf("checking = %+v, want matched by last four and syncing from the day after the file", accounts["plaid-checking"])
	}
	if accounts["plaid-savings"].AccountID != "" {
		t.Fatalf("savings matched %q with no budget-store account having 9999", accounts["plaid-savings"].AccountID)
	}

	// Linking ran a first sync, with nothing in it yet.
	if plaid.syncCalls != 1 {
		t.Fatalf("linking made %d sync calls, want one first sync", plaid.syncCalls)
	}
	plaid.pages = [][]map[string]any{
		{{"added": []any{
			plaidTxn("t-old", "", "plaid-checking", 50.00, "2026-11-03", "GROCERY", false), // the file has it
			plaidTxn("t-pend", "", "plaid-checking", 12.34, "2026-11-05", "COFFEE", true),
			plaidTxn("t-pay", "", "plaid-checking", -2000.00, "2026-11-05", "PAYROLL", false),
			plaidTxn("t-sav", "", "plaid-savings", -1.00, "2026-11-05", "INTEREST", false),
		}, "modified": []any{}, "removed": []any{}}},
	}
	first, err := s.SyncPlaid(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Added != 2 || first.BeforeSyncStart != 1 || first.UnmatchedAccount != 1 || len(first.Errors) != 0 {
		t.Fatalf("first sync = %+v", first)
	}
	if got := balance(t, s, checking.ID); got != -5000-1234+200000 {
		t.Fatalf("checking balance = %d", got)
	}
	// The pending coffee posts with a new id and a new amount; the payroll is
	// removed (the bank reversed it).
	plaid.pages = append(plaid.pages, []map[string]any{{"added": []any{
		plaidTxn("t-posted", "t-pend", "plaid-checking", 13.00, "2026-11-06", "COFFEE SHOP", false),
	}, "modified": []any{}, "removed": []any{map[string]any{"transaction_id": "t-pay"}}}})
	second, err := s.SyncPlaid(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Updated != 1 || second.Voided != 1 {
		t.Fatalf("second sync = %+v, want the coffee updated in place and the payroll voided", second)
	}
	coffee, _ := s.ListTransactions(TransactionFilter{Query: "COFFEE"})
	if len(coffee) != 1 || coffee[0].Status != "posted" || coffee[0].OccurredOn != "2026-11-06" || len(coffee[0].SourceRowIDs) != 2 {
		t.Fatalf("coffee = %+v, want one posted transaction holding both rows", coffee)
	}
	if got := balance(t, s, checking.ID); got != -5000-1300 {
		t.Fatalf("checking after the second sync = %d, want the file's grocery and the posted coffee only", got)
	}
	if plaid.syncCalls < 3 {
		t.Fatalf("the interrupted sync was not retried: %d calls", plaid.syncCalls)
	}
}

func TestPlaidCentsRoundsAndFlipsTheSign(t *testing.T) {
	for amount, want := range map[float64]int64{12.34: -1234, -2000: 200000, 0.1 + 0.2: -30, 19.999: -2000} {
		if got := plaidCents(amount); got != want {
			t.Errorf("plaidCents(%v) = %d, want %d", amount, got, want)
		}
	}
}
