package possessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Spec is one line of a listing's specifications, in the listing's words.
type Spec struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Product is a make and model as its listing describes it. The listing page
// itself is article-store's (ArticleID); a product keeps what was read from
// it and where.
type Product struct {
	ID                string   `json:"id"`
	Brand             string   `json:"brand"`
	ModelNumber       string   `json:"model_number"`
	Name              string   `json:"name"`
	ListingURL        string   `json:"listing_url"`
	ListingSource     string   `json:"listing_source"`
	ListingPublisher  string   `json:"listing_publisher"`
	ArticleID         string   `json:"article_id"`
	FoundBy           string   `json:"found_by"`
	Description       string   `json:"description"`
	Specs             []Spec   `json:"specs"`
	ListPriceCents    *int64   `json:"list_price_cents"`
	ListPriceCurrency string   `json:"list_price_currency"`
	ExtractedBy       string   `json:"extracted_by"`
	ExtractionCostUSD float64  `json:"extraction_cost_usd"`
	CreatedBy         string   `json:"created_by"`
	CreatedAt         int64    `json:"created_at"`
	UpdatedAt         int64    `json:"updated_at"`
	DeletedAt         int64    `json:"deleted_at"`
	PossessionIDs     []string `json:"possession_ids"` // the live possessions that are this product
}

// ProductRequest is POST /products. Without article_id the store saves
// listing_url in article-store first; without a description the first
// reader reads description, specs and list price from the page.
type ProductRequest struct {
	Actor             string   `json:"actor"`
	Brand             string   `json:"brand"`
	ModelNumber       string   `json:"model_number"`
	Name              string   `json:"name"`
	ListingURL        string   `json:"listing_url"`
	ListingSource     string   `json:"listing_source"`
	ListingPublisher  string   `json:"listing_publisher"`
	ArticleID         string   `json:"article_id"`
	FoundBy           string   `json:"found_by"`
	Description       string   `json:"description"`
	Specs             []Spec   `json:"specs"`
	ListPriceCents    *int64   `json:"list_price_cents"`
	ListPriceCurrency string   `json:"list_price_currency"`
	PossessionIDs     []string `json:"possession_ids"`
}

const productColumns = `id, brand, model_number, name, listing_url, listing_source, listing_publisher, article_id, found_by, description, specs,
	list_price_cents, list_price_currency, extracted_by, extraction_cost_usd, created_by, created_at, updated_at, deleted_at`

func scanProduct(row interface{ Scan(...any) error }) (Product, error) {
	var product Product
	var specs string
	var price sql.NullInt64
	err := row.Scan(&product.ID, &product.Brand, &product.ModelNumber, &product.Name, &product.ListingURL, &product.ListingSource, &product.ListingPublisher,
		&product.ArticleID, &product.FoundBy, &product.Description, &specs, &price, &product.ListPriceCurrency, &product.ExtractedBy, &product.ExtractionCostUSD,
		&product.CreatedBy, &product.CreatedAt, &product.UpdatedAt, &product.DeletedAt)
	if err != nil {
		return Product{}, err
	}
	if price.Valid {
		product.ListPriceCents = &price.Int64
	}
	if err := json.Unmarshal([]byte(specs), &product.Specs); err != nil {
		return Product{}, fmt.Errorf("product %s specs: %w", product.ID, err)
	}
	return product, nil
}

func requireLiveProduct(q querier, id string) error {
	var deletedAt int64
	err := q.QueryRow(`SELECT deleted_at FROM products WHERE id = ?`, id).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deletedAt != 0) {
		return fmt.Errorf("%w: no product %s", ErrInvalid, id)
	}
	return err
}

// GetProduct answers one product with the possessions that are it.
func (s *Store) GetProduct(id string) (Product, error) {
	product, err := scanProduct(s.db.QueryRow(`SELECT `+productColumns+` FROM products WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Product{}, fmt.Errorf("%w: no product %s", ErrNotFound, id)
	}
	if err != nil {
		return Product{}, err
	}
	product.PossessionIDs, err = stringColumn(s.db, `SELECT id FROM possessions WHERE product_id = ? AND deleted_at = 0 ORDER BY seq`, id)
	return product, err
}

// ListProducts answers live products, newest first; q matches every word in
// brand, model number, name or description.
func (s *Store) ListProducts(query string, limit, offset int) ([]Product, error) {
	conditions := []string{"deleted_at = 0"}
	arguments := []any{}
	for _, word := range strings.Fields(strings.ToLower(query)) {
		conditions = append(conditions, `strpos(lower(brand || ' ' || model_number || ' ' || name || ' ' || description), ?) > 0`)
		arguments = append(arguments, word)
	}
	if limit <= 0 {
		limit = 100
	}
	arguments = append(arguments, limit, offset)
	ids, err := stringColumn(s.db, `SELECT id FROM products WHERE `+strings.Join(conditions, " AND ")+` ORDER BY seq DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, err
	}
	products := make([]Product, 0, len(ids))
	for _, id := range ids {
		product, err := s.GetProduct(id)
		if err != nil {
			return nil, err
		}
		products = append(products, product)
	}
	return products, nil
}

func checkProductFields(product Product) error {
	if strings.TrimSpace(product.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if !slices.Contains(ListingSources, product.ListingSource) {
		return fmt.Errorf("%w: listing_source must be one of %s", ErrInvalid, strings.Join(ListingSources, ", "))
	}
	if parsed, err := url.Parse(product.ListingURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w: listing_url must be an http or https URL", ErrInvalid)
	}
	if product.ArticleID == "" {
		return fmt.Errorf("%w: article_id is required", ErrInvalid)
	}
	if product.ListPriceCents != nil && (*product.ListPriceCents < 0 || len(product.ListPriceCurrency) != 3) {
		return fmt.Errorf("%w: list_price_cents needs list_price_currency, a three-letter ISO 4217 code", ErrInvalid)
	}
	if product.ListPriceCents == nil && product.ListPriceCurrency != "" {
		return fmt.Errorf("%w: list_price_currency without list_price_cents", ErrInvalid)
	}
	return nil
}

// insertProduct records a product and points each possession at it.
func (s *Store) insertProduct(ctx context.Context, product Product, possessionIDs []string, actor string) (Product, error) {
	if actor == "" {
		return Product{}, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	if product.Specs == nil {
		product.Specs = []Spec{}
	}
	if err := checkProductFields(product); err != nil {
		return Product{}, err
	}
	if err := s.checkOutside(ctx, ReferenceArticle, product.ArticleID); err != nil {
		return Product{}, err
	}
	specs, _ := json.Marshal(product.Specs)
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Product{}, err
	}
	defer transaction.Rollback()
	if product.ModelNumber != "" {
		existing, err := stringColumn(transaction, `SELECT id FROM products WHERE lower(brand) = lower(?) AND lower(model_number) = lower(?) AND deleted_at = 0`, product.Brand, product.ModelNumber)
		if err != nil {
			return Product{}, err
		}
		if len(existing) > 0 {
			return Product{}, fmt.Errorf("%w: %s %s is already %s", ErrConflict, product.Brand, product.ModelNumber, existing[0])
		}
	}
	id, sequence, err := nextID(transaction, "products", "product")
	if err != nil {
		return Product{}, err
	}
	now := s.unixNow()
	if _, err := transaction.Exec(`INSERT INTO products (id, seq, brand, model_number, name, listing_url, listing_source, listing_publisher, article_id, found_by, description, specs,
		list_price_cents, list_price_currency, extracted_by, extraction_cost_usd, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sequence, product.Brand, product.ModelNumber, product.Name, product.ListingURL, product.ListingSource, product.ListingPublisher, product.ArticleID, product.FoundBy,
		product.Description, string(specs), product.ListPriceCents, product.ListPriceCurrency, product.ExtractedBy, product.ExtractionCostUSD, actor, now, now); err != nil {
		return Product{}, err
	}
	productField, _ := json.Marshal(id)
	for _, possessionID := range possessionIDs {
		if err := s.patchPossessionInTransaction(ctx, transaction, possessionID, map[string]json.RawMessage{"product_id": productField}, nil, nil, actor); err != nil {
			return Product{}, fmt.Errorf("link %s: %w", possessionID, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return Product{}, err
	}
	return s.GetProduct(id)
}

// PatchProduct changes what a person corrects on a product. A new
// article_id is checked against article-store.
func (s *Store) PatchProduct(ctx context.Context, id string, fields map[string]json.RawMessage, actor string) (Product, error) {
	if actor == "" {
		return Product{}, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	product, err := s.GetProduct(id)
	if err != nil {
		return Product{}, err
	}
	if product.DeletedAt != 0 {
		return Product{}, fmt.Errorf("%w: %s is deleted", ErrConflict, id)
	}
	targets := map[string]any{"brand": &product.Brand, "model_number": &product.ModelNumber, "name": &product.Name, "listing_url": &product.ListingURL,
		"listing_source": &product.ListingSource, "listing_publisher": &product.ListingPublisher, "article_id": &product.ArticleID, "found_by": &product.FoundBy,
		"description": &product.Description, "specs": &product.Specs, "list_price_cents": &product.ListPriceCents, "list_price_currency": &product.ListPriceCurrency}
	articleBefore := product.ArticleID
	for name, raw := range fields {
		target, known := targets[name]
		if !known {
			return Product{}, fmt.Errorf("%w: %s is not a field of a product that can be changed", ErrInvalid, name)
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return Product{}, fmt.Errorf("%w: %s: %v", ErrInvalid, name, err)
		}
	}
	if product.Specs == nil {
		product.Specs = []Spec{}
	}
	if err := checkProductFields(product); err != nil {
		return Product{}, err
	}
	if product.ArticleID != articleBefore {
		if err := s.checkOutside(ctx, ReferenceArticle, product.ArticleID); err != nil {
			return Product{}, err
		}
	}
	specs, _ := json.Marshal(product.Specs)
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Product{}, err
	}
	defer transaction.Rollback()
	now := s.unixNow()
	if err := saveRevision(transaction, "products", "id", id, actor, now); err != nil {
		return Product{}, err
	}
	if _, err := transaction.Exec(`UPDATE products SET brand = ?, model_number = ?, name = ?, listing_url = ?, listing_source = ?, listing_publisher = ?, article_id = ?,
		found_by = ?, description = ?, specs = ?, list_price_cents = ?, list_price_currency = ?, updated_at = ? WHERE id = ?`,
		product.Brand, product.ModelNumber, product.Name, product.ListingURL, product.ListingSource, product.ListingPublisher, product.ArticleID,
		product.FoundBy, product.Description, string(specs), product.ListPriceCents, product.ListPriceCurrency, now, id); err != nil {
		if isUniqueViolation(err) {
			return Product{}, fmt.Errorf("%w: another product is %s %s", ErrConflict, product.Brand, product.ModelNumber)
		}
		return Product{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Product{}, err
	}
	return s.GetProduct(id)
}
