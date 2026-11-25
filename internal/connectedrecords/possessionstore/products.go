package possessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
		conditions = append(conditions, `instr(lower(brand || ' ' || model_number || ' ' || name || ' ' || description), ?) > 0`)
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
	if !contains(ListingSources, product.ListingSource) {
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
	if err := s.references.Check(ctx, ReferenceArticle, product.ArticleID); err != nil {
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
		if err := s.references.Check(ctx, ReferenceArticle, product.ArticleID); err != nil {
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
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Product{}, fmt.Errorf("%w: another product is %s %s", ErrConflict, product.Brand, product.ModelNumber)
		}
		return Product{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Product{}, err
	}
	return s.GetProduct(id)
}

// productReadingSchema is what the first reader answers about a listing.
var productReadingSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["is_this_product","name","description","specs","list_price_cents","list_price_currency","why"],"properties":{
"is_this_product":{"type":"boolean","description":"Whether the page lists the product asked about (its brand and model number, alone or among its variants)"},
"name":{"type":"string","description":"The product's name as the page gives it"},
"description":{"type":"string","description":"The page's own description of the product, its sentences copied in order, joined into one to three short paragraphs; nothing the page does not say"},
"specs":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["name","value"],"properties":{"name":{"type":"string"},"value":{"type":"string"}}},
 "description":"Every specification the page states for this model, names and values as written (fix only broken spacing)"},
"list_price_cents":{"type":"integer","description":"The page's list price for this model in cents; -1 when it gives none (\"open price\", no price)"},
"list_price_currency":{"type":"string","description":"ISO 4217 code of that price; empty with -1"},
"why":{"type":"string","description":"One sentence: where on the page this came from, and which variant when the page lists several"}}}`)

const productReadingSystemPrompt = `You read a product's listing page for a person's inventory of what they own. Copy what the page says; never add facts from memory. When the page lists several models, take only what applies to the one asked about and to all models alike.`

// CreateProduct records a product from its listing: it saves the page in
// article-store when no article_id is given and, when no description is
// given, has the first reader read description, specs and list price from
// the page. Answers the product with what the reading cost.
func (reviewer *Reviewer) CreateProduct(ctx context.Context, request ProductRequest) (Product, error) {
	if request.Actor == "" {
		return Product{}, fmt.Errorf("%w: say who is writing (the actor)", ErrInvalid)
	}
	if !contains(ListingSources, request.ListingSource) {
		return Product{}, fmt.Errorf("%w: listing_source must be one of %s", ErrInvalid, strings.Join(ListingSources, ", "))
	}
	product := Product{Brand: request.Brand, ModelNumber: request.ModelNumber, Name: request.Name, ListingURL: request.ListingURL, ListingSource: request.ListingSource,
		ListingPublisher: request.ListingPublisher, ArticleID: request.ArticleID, FoundBy: request.FoundBy, Description: request.Description, Specs: request.Specs,
		ListPriceCents: request.ListPriceCents, ListPriceCurrency: request.ListPriceCurrency}
	if product.ArticleID == "" {
		if reviewer.Articles == nil {
			return Product{}, fmt.Errorf("article-store is not wired, so the listing cannot be saved")
		}
		articleID, err := reviewer.Articles.SaveListing(ctx, request.ListingURL, []string{"product-listing", "possession-store"}, "possession-store product listing")
		if err != nil {
			return Product{}, err
		}
		product.ArticleID = articleID
	}
	if product.Description == "" {
		if reviewer.Articles == nil {
			return Product{}, fmt.Errorf("article-store is not wired, so the listing cannot be read")
		}
		page, err := reviewer.Articles.ReadListing(ctx, product.ArticleID)
		if err != nil {
			return Product{}, err
		}
		prompt := fmt.Sprintf("The product: brand %q, model number %q, name %q.\nThe listing page (%s), saved as %s, titled %q:\n\n%s\n\nRead this product's description, specifications and list price from the page.",
			request.Brand, request.ModelNumber, request.Name, request.ListingURL, page.ArticleID, page.Title, clip(page.Text, 60000))
		answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, productReadingSystemPrompt, prompt, productReadingSchema, nil, 4000)
		if err != nil {
			return Product{}, fmt.Errorf("read the listing: %w", err)
		}
		var reading struct {
			IsThisProduct     bool   `json:"is_this_product"`
			Name              string `json:"name"`
			Description       string `json:"description"`
			Specs             []Spec `json:"specs"`
			ListPriceCents    int64  `json:"list_price_cents"`
			ListPriceCurrency string `json:"list_price_currency"`
			Why               string `json:"why"`
		}
		if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
			return Product{}, fmt.Errorf("read the listing: %s answered %s: %w", answer.Model, answer.Parsed, err)
		}
		if !reading.IsThisProduct {
			return Product{}, fmt.Errorf("%w: %s says %s (%s) does not list %s %s: %s", ErrInvalid, answer.Model, request.ListingURL, product.ArticleID, request.Brand, request.ModelNumber, reading.Why)
		}
		product.Description, product.Specs = reading.Description, reading.Specs
		if product.Name == "" {
			product.Name = reading.Name
		}
		if product.ListPriceCents == nil && reading.ListPriceCents >= 0 {
			product.ListPriceCents, product.ListPriceCurrency = &reading.ListPriceCents, strings.ToUpper(reading.ListPriceCurrency)
		}
		product.ExtractedBy, product.ExtractionCostUSD = answer.Model, answer.CostUSD
	}
	return reviewer.Store.insertProduct(ctx, product, request.PossessionIDs, request.Actor)
}
