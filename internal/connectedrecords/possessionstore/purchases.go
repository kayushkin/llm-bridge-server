package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PurchaseLine is one line of an order.
type PurchaseLine struct {
	ID               string   `json:"id"`
	LineNumber       int      `json:"line_number"`
	Title            string   `json:"title"`
	Quantity         int      `json:"quantity"`
	UnitPriceCents   *int64   `json:"unit_price_cents"`
	ProductReference string   `json:"product_reference"`
	Disposition      string   `json:"disposition"`
	PossessionIDs    []string `json:"possession_ids"` // the possessions that came from this line
}

// Purchase is one order with its lines.
type Purchase struct {
	ID             string         `json:"id"`
	Seller         string         `json:"seller"`
	Marketplace    string         `json:"marketplace"`
	OrderReference string         `json:"order_reference"`
	OrderedAt      int64          `json:"ordered_at"`
	TotalCents     *int64         `json:"total_cents"`
	Currency       string         `json:"currency"`
	Source         string         `json:"source"`
	CreatedAt      int64          `json:"created_at"`
	Lines          []PurchaseLine `json:"lines"`
}

// CreatePurchase records an order. An order already recorded, found by its
// marketplace and order reference, is a conflict.
func (s *Store) CreatePurchase(ctx context.Context, purchase Purchase) (Purchase, error) {
	if !contains(Marketplaces, purchase.Marketplace) {
		return Purchase{}, fmt.Errorf("%w: marketplace must be one of %s", ErrInvalid, strings.Join(Marketplaces, ", "))
	}
	if !contains(PurchaseSources, purchase.Source) {
		return Purchase{}, fmt.Errorf("%w: source must be one of %s", ErrInvalid, strings.Join(PurchaseSources, ", "))
	}
	if purchase.OrderedAt <= 0 {
		return Purchase{}, fmt.Errorf("%w: ordered_at is required", ErrInvalid)
	}
	if purchase.TotalCents != nil && len(purchase.Currency) != 3 {
		return Purchase{}, fmt.Errorf("%w: total_cents needs currency, a three-letter ISO 4217 code", ErrInvalid)
	}
	if len(purchase.Lines) == 0 {
		return Purchase{}, fmt.Errorf("%w: a purchase needs at least one line", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Purchase{}, err
	}
	defer transaction.Rollback()
	id, sequence, err := nextID(transaction, "purchases", "possession_purchase")
	if err != nil {
		return Purchase{}, err
	}
	now := s.unixNow()
	if _, err := transaction.Exec(`INSERT INTO purchases (id, seq, seller, marketplace, order_reference, ordered_at, total_cents, currency, source, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sequence, purchase.Seller, purchase.Marketplace, purchase.OrderReference, purchase.OrderedAt, purchase.TotalCents, purchase.Currency, purchase.Source, now); err != nil {
		if isUniqueViolation(err) {
			return Purchase{}, fmt.Errorf("%w: %s order %s is already recorded", ErrConflict, purchase.Marketplace, purchase.OrderReference)
		}
		return Purchase{}, err
	}
	for index, line := range purchase.Lines {
		if strings.TrimSpace(line.Title) == "" {
			return Purchase{}, fmt.Errorf("%w: line %d has no title", ErrInvalid, index+1)
		}
		if line.Quantity <= 0 {
			line.Quantity = 1
		}
		if line.Disposition == "" {
			line.Disposition = "unreviewed"
		}
		if !contains(PurchaseLineDispositions, line.Disposition) {
			return Purchase{}, fmt.Errorf("%w: line %d: disposition must be one of %s", ErrInvalid, index+1, strings.Join(PurchaseLineDispositions, ", "))
		}
		lineID, lineSequence, err := nextID(transaction, "purchase_lines", "purchase_line")
		if err != nil {
			return Purchase{}, err
		}
		if _, err := transaction.Exec(`INSERT INTO purchase_lines (id, seq, purchase_id, line_number, title, quantity, unit_price_cents, product_reference, disposition, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			lineID, lineSequence, id, index+1, line.Title, line.Quantity, line.UnitPriceCents, line.ProductReference, line.Disposition, now); err != nil {
			return Purchase{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return Purchase{}, err
	}
	return s.GetPurchase(id)
}

// GetPurchase answers one order with its lines.
func (s *Store) GetPurchase(id string) (Purchase, error) {
	var purchase Purchase
	var total sql.NullInt64
	err := s.db.QueryRow(`SELECT id, seller, marketplace, order_reference, ordered_at, total_cents, currency, source, created_at FROM purchases WHERE id = ? AND deleted_at = 0`, id).Scan(
		&purchase.ID, &purchase.Seller, &purchase.Marketplace, &purchase.OrderReference, &purchase.OrderedAt, &total, &purchase.Currency, &purchase.Source, &purchase.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Purchase{}, fmt.Errorf("%w: no purchase %s", ErrNotFound, id)
	}
	if err != nil {
		return Purchase{}, err
	}
	if total.Valid {
		purchase.TotalCents = &total.Int64
	}
	rows, err := s.db.Query(`SELECT id, line_number, title, quantity, unit_price_cents, product_reference, disposition FROM purchase_lines WHERE purchase_id = ? ORDER BY line_number`, id)
	if err != nil {
		return Purchase{}, err
	}
	defer rows.Close()
	purchase.Lines = []PurchaseLine{}
	for rows.Next() {
		var line PurchaseLine
		var price sql.NullInt64
		if err := rows.Scan(&line.ID, &line.LineNumber, &line.Title, &line.Quantity, &price, &line.ProductReference, &line.Disposition); err != nil {
			return Purchase{}, err
		}
		if price.Valid {
			line.UnitPriceCents = &price.Int64
		}
		purchase.Lines = append(purchase.Lines, line)
	}
	if err := rows.Err(); err != nil {
		return Purchase{}, err
	}
	for index := range purchase.Lines {
		if purchase.Lines[index].PossessionIDs, err = stringColumn(s.db, `SELECT id FROM possessions WHERE purchase_line_id = ? AND deleted_at = 0 ORDER BY seq`, purchase.Lines[index].ID); err != nil {
			return Purchase{}, err
		}
	}
	return purchase, nil
}

// ListPurchases answers orders, newest ordered first.
func (s *Store) ListPurchases(marketplace string, limit, offset int) ([]Purchase, error) {
	conditions := []string{"deleted_at = 0"}
	arguments := []any{}
	if marketplace != "" {
		conditions = append(conditions, "marketplace = ?")
		arguments = append(arguments, marketplace)
	}
	if limit <= 0 {
		limit = 100
	}
	arguments = append(arguments, limit, offset)
	ids, err := stringColumn(s.db, `SELECT id FROM purchases WHERE `+strings.Join(conditions, " AND ")+` ORDER BY ordered_at DESC, seq DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, err
	}
	purchases := make([]Purchase, 0, len(ids))
	for _, id := range ids {
		purchase, err := s.GetPurchase(id)
		if err != nil {
			return nil, err
		}
		purchases = append(purchases, purchase)
	}
	return purchases, nil
}

// SetPurchaseLineDisposition says what became of an order line.
func (s *Store) SetPurchaseLineDisposition(lineID, disposition, actor string) error {
	if !contains(PurchaseLineDispositions, disposition) {
		return fmt.Errorf("%w: disposition must be one of %s", ErrInvalid, strings.Join(PurchaseLineDispositions, ", "))
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := saveRevision(transaction, "purchase_lines", "id", lineID, actor, s.unixNow()); err != nil {
		return err
	}
	if _, err := transaction.Exec(`UPDATE purchase_lines SET disposition = ? WHERE id = ?`, disposition, lineID); err != nil {
		return err
	}
	return transaction.Commit()
}

// DeletePurchase soft-deletes an order, saving the row to revisions first.
// A possession that came from one of its lines is a conflict: move it
// first. Every open question asking whether one of its lines bought a
// possession is withdrawn (answered with the reason, resolution
// withdrawn). Answers the withdrawn questions' ids.
func (s *Store) DeletePurchase(ctx context.Context, id, actor, reason string) ([]string, error) {
	if actor == "" || reason == "" {
		return nil, fmt.Errorf("%w: actor and reason are required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	var deletedAt int64
	if err := transaction.QueryRow(`SELECT deleted_at FROM purchases WHERE id = ?`, id).Scan(&deletedAt); errors.Is(err, sql.ErrNoRows) || deletedAt != 0 {
		return nil, fmt.Errorf("%w: no purchase %s", ErrNotFound, id)
	} else if err != nil {
		return nil, err
	}
	holders, err := stringColumn(transaction, `SELECT id FROM possessions WHERE deleted_at = 0 AND purchase_line_id IN (SELECT id FROM purchase_lines WHERE purchase_id = ?)`, id)
	if err != nil {
		return nil, err
	}
	if len(holders) > 0 {
		return nil, fmt.Errorf("%w: %s came from it; point them elsewhere first", ErrConflict, strings.Join(holders, ", "))
	}
	lineIDs, err := stringColumn(transaction, `SELECT id FROM purchase_lines WHERE purchase_id = ?`, id)
	if err != nil {
		return nil, err
	}
	now := s.unixNow()
	withdrawn := []string{}
	resolution, _ := json.Marshal(Resolution{Action: "withdrawn", Explanation: reason})
	for _, lineID := range lineIDs {
		questions, err := stringColumn(transaction, `SELECT id FROM owner_questions WHERE answered_at = 0 AND strpos(choices, ?) > 0`, jsonField("purchase_line_id", lineID))
		if err != nil {
			return nil, err
		}
		for _, questionID := range questions {
			if err := saveRevision(transaction, "owner_questions", "id", questionID, actor, now); err != nil {
				return nil, err
			}
			if _, err := transaction.Exec(`UPDATE owner_questions SET answer_text = ?, resolution = ?, answered_by = ?, answered_at = ? WHERE id = ?`,
				"withdrawn: "+reason, string(resolution), actor, now, questionID); err != nil {
				return nil, err
			}
			withdrawn = append(withdrawn, questionID)
		}
	}
	if err := saveRevision(transaction, "purchases", "id", id, actor, now); err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(`UPDATE purchases SET deleted_at = ? WHERE id = ?`, now, id); err != nil {
		return nil, err
	}
	return withdrawn, transaction.Commit()
}
