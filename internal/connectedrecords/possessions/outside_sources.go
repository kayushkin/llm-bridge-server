package possessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var errArticleStoreNotSet = errors.New("article-store's address is not set, so listings cannot be saved or read")

// ListingPage is a product listing as article-store keeps it.
type ListingPage struct {
	ArticleID string
	Title     string
	// Text is the page as a reader sees it: readability's article text and
	// then the whole fetched page turned into lines, since readability
	// often drops a spec table.
	Text string
}

// ArticleSource saves a web page in article-store and reads it back.
type ArticleSource interface {
	SaveListing(ctx context.Context, pageURL string, tags []string, addedBy string) (string, error)
	ReadListing(ctx context.Context, articleID string) (ListingPage, error)
}

// ArticleStore is article-store over HTTP; it runs outside this server.
type ArticleStore struct {
	URL        string
	HTTPClient *http.Client
}

// SaveListing saves the page (article-store fetches it; a URL already saved
// answers the stored article) and answers its article id.
func (store ArticleStore) SaveListing(ctx context.Context, pageURL string, tags []string, addedBy string) (string, error) {
	if store.URL == "" {
		return "", errArticleStoreNotSet
	}
	body, _ := json.Marshal(map[string]any{"url": pageURL, "kind": "other", "tags": tags, "added_by": addedBy})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, store.URL+"/articles", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	answer, err := readAnswer(store.HTTPClient, request, "article-store POST /articles")
	if err != nil {
		return "", err
	}
	var saved struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &saved); err != nil || saved.ID == "" {
		return "", fmt.Errorf("article-store POST /articles answered no id: %s", answer)
	}
	return saved.ID, nil
}

// ReadListing reads the article's text and the page it was fetched from.
func (store ArticleStore) ReadListing(ctx context.Context, articleID string) (ListingPage, error) {
	if store.URL == "" {
		return ListingPage{}, errArticleStoreNotSet
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, store.URL+"/articles/"+url.PathEscape(articleID), nil)
	if err != nil {
		return ListingPage{}, err
	}
	answer, err := readAnswer(store.HTTPClient, request, "article-store GET /articles/"+articleID)
	if err != nil {
		return ListingPage{}, err
	}
	var article struct {
		Title       string `json:"title"`
		ContentText string `json:"content_text"`
	}
	if err := json.Unmarshal(answer, &article); err != nil {
		return ListingPage{}, fmt.Errorf("article-store GET /articles/%s: %w", articleID, err)
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, store.URL+"/articles/"+url.PathEscape(articleID)+"/source", nil)
	if err != nil {
		return ListingPage{}, err
	}
	source, err := readAnswer(store.HTTPClient, request, "article-store GET /articles/"+articleID+"/source")
	if err != nil {
		return ListingPage{}, err
	}
	text := "Article text:\n" + article.ContentText + "\n\nThe whole page:\n" + pageText(string(source))
	return ListingPage{ArticleID: articleID, Title: article.Title, Text: text}, nil
}

// MailMessage is one email as mailstack answers it, with what the import
// reads.
type MailMessage struct {
	ID       string
	Subject  string
	From     string
	Date     time.Time
	BodyText string // the HTML body turned into lines, else the text body
}

// MailSource reads mail. It never changes a mailbox.
type MailSource interface {
	Search(ctx context.Context, account, query, cursor string) (ids []string, next string, err error)
	Message(ctx context.Context, account, id string) (MailMessage, error)
}

// MailMessageOf is a message as possession-store reads it: the HTML body
// turned into lines when there is one, else the text body.
func MailMessageOf(id, subject, from string, date time.Time, bodyText, bodyHTML string) MailMessage {
	body := bodyText
	if bodyHTML != "" {
		body = pageText(bodyHTML)
	}
	return MailMessage{ID: id, Subject: subject, From: from, Date: date, BodyText: body}
}

// readAnswer makes the request and answers the body of a 2xx, or an
// error naming the call, the status and the body.
func readAnswer(client *http.Client, request *http.Request, call string) ([]byte, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", call, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read answer: %w", call, err)
	}
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s answered %d: %s", call, response.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

// BudgetOrder is budget-store's marketplace order (its CONTRACT.md,
// "Marketplace orders"); the store finds TransactionID itself.
type BudgetOrder struct {
	ID            string            `json:"id,omitempty"`
	Marketplace   string            `json:"marketplace"`
	OrderNumber   string            `json:"order_number"`
	Seller        string            `json:"seller"`
	OrderedAt     int64             `json:"ordered_at"`
	TotalCents    *int64            `json:"total_cents"`
	Currency      string            `json:"currency"`
	TransactionID string            `json:"transaction_id,omitempty"`
	MatchedBy     string            `json:"matched_by,omitempty"`
	MailAccountID string            `json:"mail_account_id"`
	MailMessageID string            `json:"mail_message_id"`
	ExtractedBy   string            `json:"extracted_by"`
	CreatedBy     string            `json:"created_by"`
	Lines         []BudgetOrderLine `json:"lines"`
}

// BudgetOrderLine is one line of a budget-store marketplace order.
type BudgetOrderLine struct {
	ID             string   `json:"id,omitempty"`
	OrderID        string   `json:"order_id,omitempty"`
	Title          string   `json:"title"`
	ItemNumber     string   `json:"item_number"`
	ListingURL     string   `json:"listing_url"`
	Quantity       int      `json:"quantity"`
	UnitPriceCents *int64   `json:"unit_price_cents"`
	Disposition    string   `json:"disposition,omitempty"`
	PossessionIDs  []string `json:"possession_ids,omitempty"`
}

// BudgetOrders is budget-store's marketplace orders and links.
type BudgetOrders interface {
	ListOrders(ctx context.Context) ([]BudgetOrder, error)
	CreateOrder(ctx context.Context, order BudgetOrder) (BudgetOrder, error) // ErrConflict when already recorded
	// LinkPossession records, on the order line, the possession it bought,
	// and marks the line a possession. A link already there is not an error.
	LinkPossession(ctx context.Context, lineID, possessionID, note string) error
	// OrderOfLine reads a line and the order it is part of.
	OrderOfLine(ctx context.Context, lineID string) (BudgetOrder, BudgetOrderLine, error)
}
