package budgetstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Plaid keeps the operator's bank accounts current. A bank is linked once in
// Plaid's own window (Plaid Link); Plaid hands back a public token, which this
// store trades for an access token and saves in auth-store. Each sync asks
// Plaid what changed since the last cursor and applies it: new rows become
// transactions through the same import rows every file uses, changed rows
// update theirs, removed rows void theirs, and a pending charge that posts
// keeps its one transaction.

// ErrPlaidOff is a 503: Plaid is not configured on this store.
var ErrPlaidOff = errors.New("plaid is not configured")

// PlaidKeys are the team's keys for one Plaid environment.
type PlaidKeys struct {
	ClientID string
	Secret   string
	BaseURL  string
}

// PlaidVault reads the Plaid keys and keeps each bank's access token. The
// store never holds a token in its own database, only the vault's id for it.
type PlaidVault interface {
	PlaidKeys(ctx context.Context) (PlaidKeys, error)
	SaveAccessToken(ctx context.Context, plaidItemID, institutionName, accessToken string) (credentialID string, err error)
	AccessToken(ctx context.Context, credentialID string) (string, error)
	Environment() string
}

// SetPlaidVault turns Plaid on. Without one every Plaid route is ErrPlaidOff.
func (s *Store) SetPlaidVault(vault PlaidVault) { s.plaid = vault }

// plaidCall posts one Plaid API request with the team's keys added.
func plaidCall(ctx context.Context, keys PlaidKeys, path string, body map[string]any, into any) error {
	body["client_id"] = keys.ClientID
	body["secret"] = keys.Secret
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(keys.BaseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("plaid %s: %w", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var plaidError struct {
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}
		_ = json.Unmarshal(answer, &plaidError)
		if plaidError.ErrorCode != "" {
			return &PlaidError{Path: path, Code: plaidError.ErrorCode, Message: plaidError.ErrorMessage}
		}
		return fmt.Errorf("plaid %s answered %s: %s", path, response.Status, strings.TrimSpace(string(answer)))
	}
	return json.Unmarshal(answer, into)
}

// PlaidError is Plaid's own refusal, passed through with its code.
type PlaidError struct {
	Path    string
	Code    string
	Message string
}

func (e *PlaidError) Error() string {
	return fmt.Sprintf("plaid %s: %s: %s", e.Path, e.Code, e.Message)
}

// CreatePlaidLinkToken starts Plaid Link for one more bank. redirectURI is
// the page Plaid sends the browser back to after a bank's own sign-in page;
// it must be registered in Plaid's dashboard.
func (s *Store) CreatePlaidLinkToken(ctx context.Context, redirectURI string) (string, error) {
	if s.plaid == nil {
		return "", ErrPlaidOff
	}
	keys, err := s.plaid.PlaidKeys(ctx)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"client_name":   "Personal finances",
		"user":          map[string]any{"client_user_id": "budget-store-owner"},
		"products":      []string{"transactions"},
		"country_codes": []string{"US"},
		"language":      "en",
		"transactions":  map[string]any{"days_requested": 730},
	}
	if redirectURI != "" {
		body["redirect_uri"] = redirectURI
	}
	var answer struct {
		LinkToken string `json:"link_token"`
	}
	if err := plaidCall(ctx, keys, "/link/token/create", body, &answer); err != nil {
		return "", err
	}
	return answer.LinkToken, nil
}

// PlaidItem is a linked bank with its accounts.
type PlaidItem struct {
	ID              string         `json:"id"`
	PlaidItemID     string         `json:"plaid_item_id"`
	InstitutionID   string         `json:"institution_id"`
	InstitutionName string         `json:"institution_name"`
	Environment     string         `json:"environment"`
	LastSyncedAt    int64          `json:"last_synced_at"`
	LastError       string         `json:"last_error"`
	Accounts        []PlaidAccount `json:"accounts"`
	CreatedAt       int64          `json:"created_at"`
}

// PlaidAccount is one account inside a linked bank.
type PlaidAccount struct {
	PlaidAccountID string `json:"plaid_account_id"`
	AccountID      string `json:"account_id"` // budget-store's, empty until matched
	Name           string `json:"name"`
	Mask           string `json:"mask"`
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	SyncFromOn     string `json:"sync_from_on"`
}

type plaidAccountAnswer struct {
	AccountID    string `json:"account_id"`
	Name         string `json:"name"`
	OfficialName string `json:"official_name"`
	Mask         string `json:"mask"`
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
}

// LinkPlaidItem trades Link's public token for an access token, saves the
// token in the vault, records the bank and its accounts, and matches each
// account to a budget-store account with the same last four digits when
// exactly one has them. A matched account syncs from the day after its last
// row from a downloaded file, so nothing a file already brought is counted
// twice.
func (s *Store) LinkPlaidItem(ctx context.Context, publicToken, institutionID, institutionName string) (PlaidItem, error) {
	if s.plaid == nil {
		return PlaidItem{}, ErrPlaidOff
	}
	if strings.TrimSpace(publicToken) == "" {
		return PlaidItem{}, fmt.Errorf("%w: public_token is required", ErrInvalid)
	}
	keys, err := s.plaid.PlaidKeys(ctx)
	if err != nil {
		return PlaidItem{}, err
	}
	var exchanged struct {
		AccessToken string `json:"access_token"`
		ItemID      string `json:"item_id"`
	}
	if err := plaidCall(ctx, keys, "/item/public_token/exchange", map[string]any{"public_token": publicToken}, &exchanged); err != nil {
		return PlaidItem{}, err
	}
	credentialID, err := s.plaid.SaveAccessToken(ctx, exchanged.ItemID, institutionName, exchanged.AccessToken)
	if err != nil {
		return PlaidItem{}, fmt.Errorf("save the access token: %w", err)
	}
	var accounts struct {
		Accounts []plaidAccountAnswer `json:"accounts"`
	}
	if err := plaidCall(ctx, keys, "/accounts/get", map[string]any{"access_token": exchanged.AccessToken}, &accounts); err != nil {
		return PlaidItem{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PlaidItem{}, err
	}
	defer tx.Rollback()
	seq, id, err := mintID(tx, "plaid_items", "plaid_item")
	if err != nil {
		return PlaidItem{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO plaid_items (seq, id, plaid_item_id, institution_id, institution_name, access_credential_id, environment, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, seq, id, exchanged.ItemID, institutionID, institutionName, credentialID, s.plaid.Environment(), now, now); err != nil {
		return PlaidItem{}, err
	}
	for _, a := range accounts.Accounts {
		var matched any
		syncFrom := ""
		if a.Mask != "" {
			rows, err := tx.Query(`SELECT id FROM accounts WHERE last_four = ? AND closed_on = ''`, a.Mask)
			if err != nil {
				return PlaidItem{}, err
			}
			var ids []string
			for rows.Next() {
				var accountID string
				if err := rows.Scan(&accountID); err != nil {
					rows.Close()
					return PlaidItem{}, err
				}
				ids = append(ids, accountID)
			}
			rows.Close()
			if len(ids) == 1 {
				matched = ids[0]
				if syncFrom, err = syncStartAfterFiles(tx, ids[0]); err != nil {
					return PlaidItem{}, err
				}
			}
		}
		name := a.Name
		if a.OfficialName != "" {
			name = a.OfficialName
		}
		if _, err := tx.Exec(`INSERT INTO plaid_accounts (plaid_account_id, plaid_item_id, account_id, name, mask, type, subtype, sync_from_on, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, a.AccountID, id, matched, name, a.Mask, a.Type, a.Subtype, syncFrom, now, now); err != nil {
			return PlaidItem{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PlaidItem{}, err
	}
	return s.GetPlaidItem(id)
}

// syncStartAfterFiles is the day after an account's newest row from a
// downloaded file, or "" when no file was imported (sync everything Plaid has).
func syncStartAfterFiles(q queryer, accountID string) (string, error) {
	var last string
	if err := q.QueryRow(`SELECT COALESCE(MAX(last_row_on),'') FROM import_batches WHERE account_id = ? AND undone_at = 0 AND source != 'plaid'`, accountID).Scan(&last); err != nil {
		return "", err
	}
	if last == "" {
		return "", nil
	}
	day, err := parseDay(last)
	if err != nil {
		return "", err
	}
	return day.AddDate(0, 0, 1).Format("2006-01-02"), nil
}

// MatchPlaidAccount points a Plaid account at a budget-store account (or at
// none, with an empty id). syncFromOn empty means the day after that
// account's last downloaded file row.
func (s *Store) MatchPlaidAccount(plaidAccountID, accountID, syncFromOn string) (PlaidAccount, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return PlaidAccount{}, err
	}
	defer tx.Rollback()
	var itemID string
	if err := tx.QueryRow(`SELECT plaid_item_id FROM plaid_accounts WHERE plaid_account_id = ?`, plaidAccountID).Scan(&itemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PlaidAccount{}, ErrNotFound
		}
		return PlaidAccount{}, err
	}
	var target any
	if accountID != "" {
		if err := requireExists(tx, "accounts", "account_id", accountID); err != nil {
			return PlaidAccount{}, err
		}
		target = accountID
		if syncFromOn == "" {
			if syncFromOn, err = syncStartAfterFiles(tx, accountID); err != nil {
				return PlaidAccount{}, err
			}
		}
	}
	if err := validDate("sync_from_on", syncFromOn, false); err != nil {
		return PlaidAccount{}, err
	}
	if _, err := tx.Exec(`UPDATE plaid_accounts SET account_id = ?, sync_from_on = ?, updated_at = ? WHERE plaid_account_id = ?`, target, syncFromOn, s.unixNow(), plaidAccountID); err != nil {
		return PlaidAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlaidAccount{}, err
	}
	item, err := s.GetPlaidItem(itemID)
	if err != nil {
		return PlaidAccount{}, err
	}
	for _, a := range item.Accounts {
		if a.PlaidAccountID == plaidAccountID {
			return a, nil
		}
	}
	return PlaidAccount{}, ErrNotFound
}

// GetPlaidItem returns one linked bank with its accounts.
func (s *Store) GetPlaidItem(id string) (PlaidItem, error) {
	var item PlaidItem
	err := s.db.QueryRow(`SELECT id, plaid_item_id, institution_id, institution_name, environment, last_synced_at, last_error, created_at FROM plaid_items WHERE id = ?`, id).
		Scan(&item.ID, &item.PlaidItemID, &item.InstitutionID, &item.InstitutionName, &item.Environment, &item.LastSyncedAt, &item.LastError, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaidItem{}, ErrNotFound
	}
	if err != nil {
		return PlaidItem{}, err
	}
	rows, err := s.db.Query(`SELECT plaid_account_id, COALESCE(account_id,''), name, mask, type, subtype, sync_from_on FROM plaid_accounts WHERE plaid_item_id = ? ORDER BY name`, id)
	if err != nil {
		return PlaidItem{}, err
	}
	defer rows.Close()
	item.Accounts = []PlaidAccount{}
	for rows.Next() {
		var a PlaidAccount
		if err := rows.Scan(&a.PlaidAccountID, &a.AccountID, &a.Name, &a.Mask, &a.Type, &a.Subtype, &a.SyncFromOn); err != nil {
			return PlaidItem{}, err
		}
		item.Accounts = append(item.Accounts, a)
	}
	return item, rows.Err()
}

// ListPlaidItems returns every linked bank.
func (s *Store) ListPlaidItems() ([]PlaidItem, error) {
	rows, err := s.db.Query(`SELECT id FROM plaid_items ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []PlaidItem{}
	for _, id := range ids {
		item, err := s.GetPlaidItem(id)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// plaidTransaction is the part of Plaid's transaction this store reads; the
// whole answer is kept as the import row.
type plaidTransaction struct {
	TransactionID        string  `json:"transaction_id"`
	PendingTransactionID string  `json:"pending_transaction_id"`
	AccountID            string  `json:"account_id"`
	Amount               float64 `json:"amount"` // positive: money out of the account
	Date                 string  `json:"date"`
	Name                 string  `json:"name"`
	MerchantName         string  `json:"merchant_name"`
	Pending              bool    `json:"pending"`
}

// PlaidSyncResult counts what one sync of every linked bank did.
type PlaidSyncResult struct {
	Added            int      `json:"added"`
	Updated          int      `json:"updated"`
	Voided           int      `json:"voided"`
	BeforeSyncStart  int      `json:"before_sync_start"` // older than the account's sync start: a file already brought it
	UnmatchedAccount int      `json:"unmatched_account"` // the Plaid account is not matched to a budget-store account
	Errors           []string `json:"errors"`            // one per bank that failed; the others still synced
}

// SyncPlaid asks every linked bank what changed and applies it. One bank's
// failure is recorded on it and reported, and the others still sync.
func (s *Store) SyncPlaid(ctx context.Context) (PlaidSyncResult, error) {
	result := PlaidSyncResult{Errors: []string{}}
	if s.plaid == nil {
		return result, ErrPlaidOff
	}
	keys, err := s.plaid.PlaidKeys(ctx)
	if err != nil {
		return result, err
	}
	items, err := s.ListPlaidItems()
	if err != nil {
		return result, err
	}
	for _, item := range items {
		if err := s.syncPlaidItem(ctx, keys, item.ID, &result); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s (%s): %v", item.ID, item.InstitutionName, err))
			if _, dbErr := s.db.Exec(`UPDATE plaid_items SET last_error = ?, updated_at = ? WHERE id = ?`, err.Error(), s.unixNow(), item.ID); dbErr != nil {
				return result, dbErr
			}
		}
	}
	return result, nil
}

func (s *Store) syncPlaidItem(ctx context.Context, keys PlaidKeys, itemID string, result *PlaidSyncResult) error {
	var credentialID, cursor string
	if err := s.db.QueryRow(`SELECT access_credential_id, sync_cursor FROM plaid_items WHERE id = ?`, itemID).Scan(&credentialID, &cursor); err != nil {
		return err
	}
	accessToken, err := s.plaid.AccessToken(ctx, credentialID)
	if err != nil {
		return err
	}
	// Read every page first: Plaid asks a caller to restart from the first
	// cursor when the data changes mid-way, so nothing is written until the
	// whole set of changes is in hand.
	var added, modified []json.RawMessage
	var removed []string
	next := cursor
	for attempt := 0; ; {
		var page struct {
			Added    []json.RawMessage `json:"added"`
			Modified []json.RawMessage `json:"modified"`
			Removed  []struct {
				TransactionID string `json:"transaction_id"`
			} `json:"removed"`
			NextCursor string `json:"next_cursor"`
			HasMore    bool   `json:"has_more"`
		}
		err := plaidCall(ctx, keys, "/transactions/sync", map[string]any{"access_token": accessToken, "cursor": next, "count": 500}, &page)
		var plaidErr *PlaidError
		if errors.As(err, &plaidErr) && plaidErr.Code == "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION" && attempt < 3 {
			attempt++
			added, modified, removed, next = nil, nil, nil, cursor
			continue
		}
		if err != nil {
			return err
		}
		added = append(added, page.Added...)
		modified = append(modified, page.Modified...)
		for _, r := range page.Removed {
			removed = append(removed, r.TransactionID)
		}
		next = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accounts := map[string]PlaidAccount{}
	rows, err := tx.Query(`SELECT plaid_account_id, COALESCE(account_id,''), sync_from_on FROM plaid_accounts WHERE plaid_item_id = ?`, itemID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a PlaidAccount
		if err := rows.Scan(&a.PlaidAccountID, &a.AccountID, &a.SyncFromOn); err != nil {
			rows.Close()
			return err
		}
		accounts[a.PlaidAccountID] = a
	}
	rows.Close()
	batches := map[string]string{} // budget-store account → this sync's batch
	for _, raw := range append(added, modified...) {
		var t plaidTransaction
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("read a Plaid transaction: %w", err)
		}
		account, found := accounts[t.AccountID]
		if !found || account.AccountID == "" {
			result.UnmatchedAccount++
			continue
		}
		if account.SyncFromOn != "" && t.Date < account.SyncFromOn {
			result.BeforeSyncStart++
			continue
		}
		outcome, err := s.applyPlaidTransaction(tx, batches, account.AccountID, t, raw)
		if err != nil {
			return fmt.Errorf("apply %s: %w", t.TransactionID, err)
		}
		switch outcome {
		case "added":
			result.Added++
		case "updated":
			result.Updated++
		}
	}
	for _, transactionID := range removed {
		changed, err := tx.Exec(`UPDATE transactions SET status = 'void', updated_at = ? WHERE id IN (
			SELECT ts.transaction_id FROM transaction_sources ts JOIN import_rows r ON r.id = ts.import_row_id WHERE r.dedupe_key = ?) AND status != 'void'`,
			s.unixNow(), "plaid:"+transactionID)
		if err != nil {
			return err
		}
		if n, _ := changed.RowsAffected(); n > 0 {
			result.Voided += int(n)
		}
	}
	if _, err := tx.Exec(`UPDATE plaid_items SET sync_cursor = ?, last_synced_at = ?, last_error = '', updated_at = ? WHERE id = ?`, next, s.unixNow(), s.unixNow(), itemID); err != nil {
		return err
	}
	return tx.Commit()
}

// plaidCents turns Plaid's amount (positive is money out) into this store's
// sign (a change in the account's balance), rounding to the cent.
func plaidCents(amount float64) int64 {
	cents := amount * 100
	if cents >= 0 {
		return -int64(cents + 0.5)
	}
	return -int64(cents - 0.5)
}

// applyPlaidTransaction adds a Plaid transaction, or updates the one it
// already made (the same transaction_id, or the pending one a posted charge
// replaces). Answers "added" or "updated".
func (s *Store) applyPlaidTransaction(tx queryer, batches map[string]string, accountID string, t plaidTransaction, raw json.RawMessage) (string, error) {
	if err := validDate("date", t.Date, true); err != nil {
		return "", err
	}
	amount := plaidCents(t.Amount)
	status := "posted"
	if t.Pending {
		status = "pending"
	}
	description := strings.Join(strings.Fields(t.Name), " ")
	batchID, err := s.plaidBatch(tx, batches, accountID)
	if err != nil {
		return "", err
	}
	rowSeq, rowID, err := mintID(tx, "import_rows", "import_row")
	if err != nil {
		return "", err
	}
	var rowNumber int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(row_number),0) + 1 FROM import_rows WHERE import_batch_id = ?`, batchID).Scan(&rowNumber); err != nil {
		return "", err
	}
	key := "plaid:" + t.TransactionID
	// The transaction this row continues: its own earlier version, or the
	// pending charge it posts.
	var existing string
	for _, earlierKey := range []string{key, "plaid:" + t.PendingTransactionID} {
		if earlierKey == "plaid:" {
			continue
		}
		err := tx.QueryRow(`SELECT ts.transaction_id FROM transaction_sources ts JOIN import_rows r ON r.id = ts.import_row_id
			JOIN transactions t ON t.id = ts.transaction_id WHERE r.dedupe_key = ? ORDER BY r.seq DESC LIMIT 1`, earlierKey).Scan(&existing)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	outcome := "created"
	if existing != "" {
		outcome = "updated"
	}
	if _, err := tx.Exec(`INSERT INTO import_rows (seq, id, import_batch_id, row_number, dedupe_key, outcome, raw_json) VALUES (?,?,?,?,?,?,?)`,
		rowSeq, rowID, batchID, rowNumber, key, outcome, string(raw)); err != nil {
		return "", err
	}
	now := s.unixNow()
	if existing != "" {
		// Change the amount on the account and on the category side when the
		// transaction is still a plain two-posting row; a split or a paired
		// transfer keeps its postings and only takes the new status and date.
		var postings int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM postings WHERE transaction_id = ?`, existing).Scan(&postings); err != nil {
			return "", err
		}
		if postings == 2 {
			if _, err := tx.Exec(`UPDATE postings SET amount_cents = CASE WHEN account_id IS NOT NULL THEN ? ELSE ? END WHERE transaction_id = ?`, amount, -amount, existing); err != nil {
				return "", err
			}
		}
		if _, err := tx.Exec(`UPDATE transactions SET status = ?, occurred_on = ?, description = ?, updated_at = ? WHERE id = ?`, status, t.Date, description, now, existing); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO transaction_sources (transaction_id, import_row_id) VALUES (?,?)`, existing, rowID); err != nil {
			return "", err
		}
		return "updated", nil
	}
	uncategorized, err := categoryIDForRole(tx, "uncategorized")
	if err != nil {
		return "", err
	}
	rules, err := loadPayeeRules(tx)
	if err != nil {
		return "", err
	}
	categoryID, payeeID := uncategorized, ""
	if rule, matched := matchPayee(rules, description); matched {
		payeeID = rule.payeeID
		if rule.defaultCategoryID != "" {
			categoryID = rule.defaultCategoryID
		}
	}
	transactionID, err := s.insertTransaction(tx, TransactionInput{
		OccurredOn: t.Date, Status: status, PayeeID: payeeID, Description: description,
		Postings: []Posting{{AccountID: accountID, AmountCents: amount}, {CategoryID: categoryID, AmountCents: -amount}},
	})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO transaction_sources (transaction_id, import_row_id) VALUES (?,?)`, transactionID, rowID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE import_batches SET first_row_on = CASE WHEN first_row_on = '' OR first_row_on > ? THEN ? ELSE first_row_on END,
		last_row_on = CASE WHEN last_row_on < ? THEN ? ELSE last_row_on END WHERE id = ?`, t.Date, t.Date, t.Date, t.Date, batchID); err != nil {
		return "", err
	}
	return "added", nil
}

// plaidBatch is the import batch this sync's rows for one account go into,
// made on first use.
func (s *Store) plaidBatch(tx queryer, batches map[string]string, accountID string) (string, error) {
	if id, found := batches[accountID]; found {
		return id, nil
	}
	seq, id, err := mintID(tx, "import_batches", "import_batch")
	if err != nil {
		return "", err
	}
	now := s.unixNow()
	stamp := sha256.Sum256([]byte(fmt.Sprintf("plaid sync %s %d %s", accountID, s.now().UnixNano(), id)))
	if _, err := tx.Exec(`INSERT INTO import_batches (seq, id, source, account_id, file_name, file_id, file_sha256, imported_at) VALUES (?,?,'plaid',?,?,'',?,?)`,
		seq, id, accountID, "plaid sync "+time.Unix(now, 0).UTC().Format(time.RFC3339), hex.EncodeToString(stamp[:]), now); err != nil {
		return "", err
	}
	batches[accountID] = id
	return id, nil
}

// AuthStoreVault keeps Plaid's keys and access tokens in auth-store.
type AuthStoreVault struct {
	BaseURL          string
	Token            string
	PlaidEnvironment string
	Client           *http.Client
}

// Environment is the Plaid environment whose keys this vault reads.
func (v AuthStoreVault) Environment() string { return v.PlaidEnvironment }

func (v AuthStoreVault) call(ctx context.Context, method, path string, body any, into any, reason string) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+v.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-App", "budget-store")
	request.Header.Set("X-Auth-Reason", reason)
	response, err := v.Client.Do(request)
	if err != nil {
		return fmt.Errorf("auth-store: %w", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("auth-store %s %s answered %s: %s", method, path, response.Status, strings.TrimSpace(string(answer)))
	}
	return json.Unmarshal(answer, into)
}

type authStoreSecret struct {
	ID       string `json:"id"`
	ClientID string `json:"client_id"`
	APIKey   string `json:"api_key"`
	BaseURL  string `json:"base_url"`
}

// PlaidKeys reads the plaid credential whose account is the environment.
func (v AuthStoreVault) PlaidKeys(ctx context.Context) (PlaidKeys, error) {
	var secret authStoreSecret
	if err := v.call(ctx, http.MethodGet, "/api/resolve/plaid?account="+url.QueryEscape(v.PlaidEnvironment), nil, &secret, "budget-store syncs the operator's bank transactions through Plaid"); err != nil {
		return PlaidKeys{}, err
	}
	if secret.ClientID == "" || secret.APIKey == "" || secret.BaseURL == "" {
		return PlaidKeys{}, fmt.Errorf("auth-store's plaid credential for %q needs client_id, api_key (the secret) and base_url", v.PlaidEnvironment)
	}
	return PlaidKeys{ClientID: secret.ClientID, Secret: secret.APIKey, BaseURL: secret.BaseURL}, nil
}

// SaveAccessToken stores one bank's access token as its own credential.
func (v AuthStoreVault) SaveAccessToken(ctx context.Context, plaidItemID, institutionName, accessToken string) (string, error) {
	var saved struct {
		ID string `json:"id"`
	}
	err := v.call(ctx, http.MethodPost, "/api/credentials", map[string]any{
		"provider": "plaid-item", "owner": "budget-store", "account": plaidItemID, "auth_type": "api_key", "api_key": accessToken,
		"label": fmt.Sprintf("Plaid access token for %s (%s), linked through budget-store", institutionName, v.PlaidEnvironment),
	}, &saved, "budget-store saves a newly linked bank's Plaid access token")
	if err != nil {
		return "", err
	}
	return saved.ID, nil
}

// AccessToken reads one bank's access token back.
func (v AuthStoreVault) AccessToken(ctx context.Context, credentialID string) (string, error) {
	var secret authStoreSecret
	if err := v.call(ctx, http.MethodGet, "/api/credentials/"+url.PathEscape(credentialID)+"/resolve", nil, &secret, "budget-store syncs a linked bank's transactions"); err != nil {
		return "", err
	}
	if secret.APIKey == "" {
		return "", fmt.Errorf("auth-store credential %s holds no access token", credentialID)
	}
	return secret.APIKey, nil
}
