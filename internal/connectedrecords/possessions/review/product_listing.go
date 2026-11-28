package review

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"slices"
	"strings"
)

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
func (reviewer *Reviewer) CreateProduct(ctx context.Context, request possessions.ProductRequest) (possessions.Product, error) {
	if request.Actor == "" {
		return possessions.Product{}, fmt.Errorf("%w: say who is writing (the actor)", possessions.ErrInvalid)
	}
	if !slices.Contains(possessions.ListingSources, request.ListingSource) {
		return possessions.Product{}, fmt.Errorf("%w: listing_source must be one of %s", possessions.ErrInvalid, strings.Join(possessions.ListingSources, ", "))
	}
	product := possessions.Product{Brand: request.Brand, ModelNumber: request.ModelNumber, Name: request.Name, ListingURL: request.ListingURL, ListingSource: request.ListingSource,
		ListingPublisher: request.ListingPublisher, ArticleID: request.ArticleID, FoundBy: request.FoundBy, Description: request.Description, Specs: request.Specs,
		ListPriceCents: request.ListPriceCents, ListPriceCurrency: request.ListPriceCurrency}
	if product.ArticleID == "" {
		if reviewer.Articles == nil {
			return possessions.Product{}, fmt.Errorf("article-store is not wired, so the listing cannot be saved")
		}
		articleID, err := reviewer.Articles.SaveListing(ctx, request.ListingURL, []string{"product-listing", "possession-store"}, "possession-store product listing")
		if err != nil {
			return possessions.Product{}, err
		}
		product.ArticleID = articleID
	}
	if product.Description == "" {
		if reviewer.Articles == nil {
			return possessions.Product{}, fmt.Errorf("article-store is not wired, so the listing cannot be read")
		}
		page, err := reviewer.Articles.ReadListing(ctx, product.ArticleID)
		if err != nil {
			return possessions.Product{}, err
		}
		prompt := fmt.Sprintf("The product: brand %q, model number %q, name %q.\nThe listing page (%s), saved as %s, titled %q:\n\n%s\n\nRead this product's description, specifications and list price from the page.",
			request.Brand, request.ModelNumber, request.Name, request.ListingURL, page.ArticleID, page.Title, possessions.Clip(page.Text, 60000))
		answer, err := reviewer.Models.Ask(ctx, reviewer.FirstReaderRole, productReadingSystemPrompt, prompt, productReadingSchema, nil, 4000)
		if err != nil {
			return possessions.Product{}, fmt.Errorf("read the listing: %w", err)
		}
		var reading struct {
			IsThisProduct     bool               `json:"is_this_product"`
			Name              string             `json:"name"`
			Description       string             `json:"description"`
			Specs             []possessions.Spec `json:"specs"`
			ListPriceCents    int64              `json:"list_price_cents"`
			ListPriceCurrency string             `json:"list_price_currency"`
			Why               string             `json:"why"`
		}
		if err := json.Unmarshal(answer.Parsed, &reading); err != nil {
			return possessions.Product{}, fmt.Errorf("read the listing: %s answered %s: %w", answer.Model, answer.Parsed, err)
		}
		if !reading.IsThisProduct {
			return possessions.Product{}, fmt.Errorf("%w: %s says %s (%s) does not list %s %s: %s", possessions.ErrInvalid, answer.Model, request.ListingURL, product.ArticleID, request.Brand, request.ModelNumber, reading.Why)
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
	return reviewer.Store.InsertProduct(ctx, product, request.PossessionIDs, request.Actor)
}
