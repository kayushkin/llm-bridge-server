package budgetstore

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Marketplaces are where an order can be placed.
var marketplaces = []string{"ebay"}

// orderMatchMethods say how an order found its transaction: the order number
// printed in the bank's description, or, failing that, the one charge of
// the order's total within a week of it whose description names the
// marketplace.
var orderMatchMethods = []string{"order_number", "amount_and_date"}

// orderLineDispositions say what became of what a line bought. possession:
// possession-store holds it (a link from the line names the possession).
var orderLineDispositions = []string{"unreviewed", "possession", "consumable", "returned", "gift_for_someone_else"}

// marketplaceDescriptionMarkers are how each marketplace's charges read in
// a bank's description, upper-cased.
var marketplaceDescriptionMarkers = map[string]string{"ebay": "EBAY"}

// orderMatchWindowDays is how long after the order its charge may post.
const orderMatchWindowDays = 7

// MarketplaceOrder is one order and its lines.
type MarketplaceOrder struct {
	ID            string                 `json:"id"`
	Marketplace   string                 `json:"marketplace"`
	OrderNumber   string                 `json:"order_number"`
	Seller        string                 `json:"seller"`
	OrderedAt     int64                  `json:"ordered_at"`
	TotalCents    *int64                 `json:"total_cents"`
	Currency      string                 `json:"currency"`
	TransactionID string                 `json:"transaction_id"` // '' until a charge is found
	MatchedBy     string                 `json:"matched_by"`
	MailAccountID string                 `json:"mail_account_id"`
	MailMessageID string                 `json:"mail_message_id"`
	ExtractedBy   string                 `json:"extracted_by"`
	CreatedBy     string                 `json:"created_by"`
	CreatedAt     int64                  `json:"created_at"`
	UpdatedAt     int64                  `json:"updated_at"`
	Lines         []MarketplaceOrderLine `json:"lines"`
}

// MarketplaceOrderLine is one thing an order bought. PossessionIDs are the
// possession-store records links from the line name.
type MarketplaceOrderLine struct {
	ID             string   `json:"id"`
	OrderID        string   `json:"order_id"`
	LineNumber     int      `json:"line_number"`
	Title          string   `json:"title"`
	ItemNumber     string   `json:"item_number"`
	ListingURL     string   `json:"listing_url"`
	Quantity       int      `json:"quantity"`
	UnitPriceCents *int64   `json:"unit_price_cents"`
	Disposition    string   `json:"disposition"`
	PossessionIDs  []string `json:"possession_ids"`
}

const marketplaceOrderColumns = `id, marketplace, order_number, seller, ordered_at, total_cents, currency, COALESCE(transaction_id,''), matched_by,
	mail_account_id, mail_message_id, extracted_by, created_by, created_at, updated_at`

// CreateMarketplaceOrder records an order and looks for its charge. An
// order already recorded (the same marketplace and order number, or the
// same email) is a conflict naming it.
func (s *Ledger) CreateMarketplaceOrder(in MarketplaceOrder) (MarketplaceOrder, error) {
	if !slices.Contains(marketplaces, in.Marketplace) {
		return MarketplaceOrder{}, fmt.Errorf("%w: marketplace must be one of %s", ErrInvalid, strings.Join(marketplaces, ", "))
	}
	if in.OrderedAt <= 0 {
		return MarketplaceOrder{}, fmt.Errorf("%w: ordered_at is required", ErrInvalid)
	}
	if len(in.Currency) != 3 || strings.ToUpper(in.Currency) != in.Currency {
		return MarketplaceOrder{}, fmt.Errorf("%w: currency must be a three-letter ISO 4217 code in capitals", ErrInvalid)
	}
	if strings.TrimSpace(in.CreatedBy) == "" {
		return MarketplaceOrder{}, fmt.Errorf("%w: created_by is required", ErrInvalid)
	}
	if (in.MailAccountID == "") != (in.MailMessageID == "") {
		return MarketplaceOrder{}, fmt.Errorf("%w: mail_account_id and mail_message_id go together", ErrInvalid)
	}
	if in.TransactionID != "" || in.MatchedBy != "" {
		return MarketplaceOrder{}, fmt.Errorf("%w: the store finds the transaction itself; leave transaction_id and matched_by out", ErrInvalid)
	}
	if len(in.Lines) == 0 {
		return MarketplaceOrder{}, fmt.Errorf("%w: an order needs at least one line", ErrInvalid)
	}
	for index, line := range in.Lines {
		if strings.TrimSpace(line.Title) == "" {
			return MarketplaceOrder{}, fmt.Errorf("%w: line %d has no title", ErrInvalid, index+1)
		}
		if line.ListingURL != "" {
			if parsed, err := url.Parse(line.ListingURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return MarketplaceOrder{}, fmt.Errorf("%w: line %d: listing_url must be an http or https URL", ErrInvalid, index+1)
			}
		}
		if line.Disposition != "" && !slices.Contains(orderLineDispositions, line.Disposition) {
			return MarketplaceOrder{}, fmt.Errorf("%w: line %d: disposition must be one of %s", ErrInvalid, index+1, strings.Join(orderLineDispositions, ", "))
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return MarketplaceOrder{}, err
	}
	defer tx.Rollback()
	for _, existing := range []struct{ query, value, what string }{
		{`SELECT id FROM marketplace_orders WHERE marketplace = $1 AND order_number = $2 AND $3 != ''`, in.OrderNumber, "order " + in.OrderNumber},
		{`SELECT id FROM marketplace_orders WHERE mail_account_id || ':' || mail_message_id = $1 AND $2 != ':'`, in.MailAccountID + ":" + in.MailMessageID, "the order in mail " + in.MailAccountID + ":" + in.MailMessageID},
	} {
		var id string
		arguments := []any{existing.value, existing.value}
		if strings.Contains(existing.query, "marketplace = $1") {
			arguments = []any{in.Marketplace, existing.value, existing.value}
		}
		err := tx.QueryRow(existing.query, arguments...).Scan(&id)
		if err == nil {
			return MarketplaceOrder{}, fmt.Errorf("%w: %s is already %s", ErrConflict, existing.what, id)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return MarketplaceOrder{}, err
		}
	}
	seq, id, err := mintID(tx, "marketplace_orders", "marketplace_order")
	if err != nil {
		return MarketplaceOrder{}, err
	}
	now := s.unixNow()
	if _, err := tx.Exec(`INSERT INTO marketplace_orders (seq, id, marketplace, order_number, seller, ordered_at, total_cents, currency, mail_account_id, mail_message_id, extracted_by, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, seq, id, in.Marketplace, in.OrderNumber, in.Seller, in.OrderedAt, in.TotalCents, in.Currency,
		in.MailAccountID, in.MailMessageID, in.ExtractedBy, in.CreatedBy, now, now); err != nil {
		return MarketplaceOrder{}, err
	}
	for index, line := range in.Lines {
		lineSeq, lineID, err := mintID(tx, "marketplace_order_lines", "marketplace_order_line")
		if err != nil {
			return MarketplaceOrder{}, err
		}
		if line.Quantity <= 0 {
			line.Quantity = 1
		}
		if line.Disposition == "" {
			line.Disposition = "unreviewed"
		}
		if _, err := tx.Exec(`INSERT INTO marketplace_order_lines (seq, id, order_id, line_number, title, item_number, listing_url, quantity, unit_price_cents, disposition) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			lineSeq, lineID, id, index+1, line.Title, line.ItemNumber, line.ListingURL, line.Quantity, line.UnitPriceCents, line.Disposition); err != nil {
			return MarketplaceOrder{}, err
		}
	}
	if _, err := matchOrderTransaction(tx, id, now); err != nil {
		return MarketplaceOrder{}, err
	}
	if err := tx.Commit(); err != nil {
		return MarketplaceOrder{}, err
	}
	return s.GetMarketplaceOrder(id)
}

// matchOrderTransaction finds the charge of an order with no transaction
// yet: the one live transaction whose description carries the order number;
// failing that, the one whose account postings sum to minus the order's USD
// total, dated from the order's day to a week after, whose description
// names the marketplace. Answers the method, or "" when none or several fit.
func matchOrderTransaction(tx queryer, orderID string, now int64) (string, error) {
	var marketplace, orderNumber, currency string
	var orderedAt int64
	var total sql.NullInt64
	if err := tx.QueryRow(`SELECT marketplace, order_number, ordered_at, total_cents, currency FROM marketplace_orders WHERE id = $1 AND transaction_id IS NULL`, orderID).
		Scan(&marketplace, &orderNumber, &orderedAt, &total, &currency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	method := ""
	candidates := []string{}
	if orderNumber != "" {
		ids, err := stringColumn(tx, `SELECT id FROM transactions WHERE strpos(upper(description), upper($1)) > 0 AND status != 'void'`, orderNumber)
		if err != nil {
			return "", err
		}
		candidates, method = ids, "order_number"
	}
	if len(candidates) != 1 && total.Valid && currency == "USD" {
		// The confirmation's time is the operator's evening at the latest, so
		// its UTC day may be the next; start a day early.
		from := time.Unix(orderedAt, 0).UTC().AddDate(0, 0, -1).Format("2006-01-02")
		to := time.Unix(orderedAt, 0).UTC().AddDate(0, 0, orderMatchWindowDays).Format("2006-01-02")
		ids, err := stringColumn(tx, `SELECT t.id FROM transactions t JOIN postings p ON p.transaction_id = t.id AND p.account_id IS NOT NULL
			WHERE t.occurred_on BETWEEN $1 AND $2 AND strpos(upper(t.description), $3) > 0 AND t.status != 'void'
			AND NOT EXISTS (SELECT 1 FROM marketplace_orders o WHERE o.transaction_id = t.id)
			GROUP BY t.id HAVING SUM(p.amount_cents) = $4`, from, to, marketplaceDescriptionMarkers[marketplace], -total.Int64)
		if err != nil {
			return "", err
		}
		candidates, method = ids, "amount_and_date"
	}
	if len(candidates) != 1 {
		return "", nil
	}
	if _, err := tx.Exec(`UPDATE marketplace_orders SET transaction_id = $1, matched_by = $2, updated_at = $3 WHERE id = $4`, candidates[0], method, now, orderID); err != nil {
		return "", err
	}
	return method, nil
}

// MatchMarketplaceOrders looks again for the charge of every order without
// one, after a new bank file is imported. Answers how many matched each way
// and how many still have none.
func (s *Ledger) MatchMarketplaceOrders() (map[string]int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := stringColumn(tx, `SELECT id FROM marketplace_orders WHERE transaction_id IS NULL ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{"order_number": 0, "amount_and_date": 0, "unmatched": 0}
	now := s.unixNow()
	for _, id := range ids {
		method, err := matchOrderTransaction(tx, id, now)
		if err != nil {
			return nil, err
		}
		if method == "" {
			method = "unmatched"
		}
		counts[method]++
	}
	return counts, tx.Commit()
}

// GetMarketplaceOrder answers one order with its lines.
func (s *Ledger) GetMarketplaceOrder(id string) (MarketplaceOrder, error) {
	orders, err := s.marketplaceOrders(`WHERE id = $1`, id)
	if err != nil {
		return MarketplaceOrder{}, err
	}
	if len(orders) == 0 {
		return MarketplaceOrder{}, fmt.Errorf("%w: no marketplace order %s", ErrNotFound, id)
	}
	return orders[0], nil
}

// MarketplaceOrderFilter narrows ListMarketplaceOrders.
type MarketplaceOrderFilter struct {
	TransactionID string
	Unmatched     bool
	Limit, Offset int
}

// ListMarketplaceOrders answers orders, newest first.
func (s *Ledger) ListMarketplaceOrders(filter MarketplaceOrderFilter) ([]MarketplaceOrder, error) {
	var arguments arguments
	conditions := []string{"1=1"}
	if filter.TransactionID != "" {
		conditions = append(conditions, "transaction_id = "+arguments.add(filter.TransactionID))
	}
	if filter.Unmatched {
		conditions = append(conditions, "transaction_id IS NULL")
	}
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 200
	}
	return s.marketplaceOrders(`WHERE `+strings.Join(conditions, " AND ")+` ORDER BY ordered_at DESC, seq DESC LIMIT `+arguments.add(filter.Limit)+` OFFSET `+arguments.add(filter.Offset), arguments.values...)
}

func (s *Ledger) marketplaceOrders(where string, arguments ...any) ([]MarketplaceOrder, error) {
	rows, err := s.db.Query(`SELECT `+marketplaceOrderColumns+` FROM marketplace_orders `+where, arguments...)
	if err != nil {
		return nil, err
	}
	orders := []MarketplaceOrder{}
	for rows.Next() {
		var order MarketplaceOrder
		var total sql.NullInt64
		if err := rows.Scan(&order.ID, &order.Marketplace, &order.OrderNumber, &order.Seller, &order.OrderedAt, &total, &order.Currency, &order.TransactionID,
			&order.MatchedBy, &order.MailAccountID, &order.MailMessageID, &order.ExtractedBy, &order.CreatedBy, &order.CreatedAt, &order.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if total.Valid {
			order.TotalCents = &total.Int64
		}
		orders = append(orders, order)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range orders {
		if orders[index].Lines, err = s.marketplaceOrderLines(`WHERE order_id = $1 ORDER BY line_number`, orders[index].ID); err != nil {
			return nil, err
		}
	}
	return orders, nil
}

func (s *Ledger) marketplaceOrderLines(where string, arguments ...any) ([]MarketplaceOrderLine, error) {
	rows, err := s.db.Query(`SELECT id, order_id, line_number, title, item_number, listing_url, quantity, unit_price_cents, disposition FROM marketplace_order_lines `+where, arguments...)
	if err != nil {
		return nil, err
	}
	lines := []MarketplaceOrderLine{}
	for rows.Next() {
		var line MarketplaceOrderLine
		var price sql.NullInt64
		if err := rows.Scan(&line.ID, &line.OrderID, &line.LineNumber, &line.Title, &line.ItemNumber, &line.ListingURL, &line.Quantity, &price, &line.Disposition); err != nil {
			rows.Close()
			return nil, err
		}
		if price.Valid {
			line.UnitPriceCents = &price.Int64
		}
		lines = append(lines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range lines {
		if lines[index].PossessionIDs, err = stringColumn(s.db, `SELECT entity_ref FROM links WHERE from_id = $1 AND entity_type = 'possession' ORDER BY seq`, lines[index].ID); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// GetMarketplaceOrderLine answers one line.
func (s *Ledger) GetMarketplaceOrderLine(id string) (MarketplaceOrderLine, error) {
	lines, err := s.marketplaceOrderLines(`WHERE id = $1`, id)
	if err != nil {
		return MarketplaceOrderLine{}, err
	}
	if len(lines) == 0 {
		return MarketplaceOrderLine{}, fmt.Errorf("%w: no marketplace order line %s", ErrNotFound, id)
	}
	return lines[0], nil
}

// SetOrderLineDisposition says what became of what a line bought.
func (s *Ledger) SetOrderLineDisposition(id, disposition string) (MarketplaceOrderLine, error) {
	if !slices.Contains(orderLineDispositions, disposition) {
		return MarketplaceOrderLine{}, fmt.Errorf("%w: disposition must be one of %s", ErrInvalid, strings.Join(orderLineDispositions, ", "))
	}
	result, err := s.db.Exec(`UPDATE marketplace_order_lines SET disposition = $1 WHERE id = $2`, disposition, id)
	if err != nil {
		return MarketplaceOrderLine{}, err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return MarketplaceOrderLine{}, fmt.Errorf("%w: no marketplace order line %s", ErrNotFound, id)
	}
	return s.GetMarketplaceOrderLine(id)
}

// stringColumn answers the first column of every row the query finds.
func stringColumn(q queryer, query string, arguments ...any) ([]string, error) {
	rows, err := q.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
