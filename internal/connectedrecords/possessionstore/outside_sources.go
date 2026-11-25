package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// Mailstack is mailstack's API at URL. HTTPClient's transport answers
// mailstack's in-process host as the caller on the request's context, so
// mail is read only from the accounts that caller may read.
type Mailstack struct {
	URL        string
	HTTPClient *http.Client
}

func (mail Mailstack) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, mail.URL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return readAnswer(mail.HTTPClient, request, "mailstack GET "+path)
}

// Search answers one page of the ids the provider's search finds.
func (mail Mailstack) Search(ctx context.Context, account, query, cursor string) ([]string, string, error) {
	answer, err := mail.get(ctx, "/api/messages", url.Values{"account": {account}, "q": {query}, "cursor": {cursor}, "limit": {strconv.Itoa(100)}})
	if err != nil {
		return nil, "", err
	}
	var page struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(answer, &page); err != nil {
		return nil, "", fmt.Errorf("mailstack GET /api/messages: %w", err)
	}
	ids := make([]string, 0, len(page.Messages))
	for _, message := range page.Messages {
		ids = append(ids, message.ID)
	}
	return ids, page.NextCursor, nil
}

// Message reads one message whole. Reading does not mark it read.
func (mail Mailstack) Message(ctx context.Context, account, id string) (MailMessage, error) {
	answer, err := mail.get(ctx, "/api/messages/"+url.PathEscape(id), url.Values{"account": {account}})
	if err != nil {
		return MailMessage{}, err
	}
	var message struct {
		BodyHTML string `json:"body_html"`
		BodyText string `json:"body_text"`
		Meta     struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Date    string `json:"date"`
			From    struct {
				Email string `json:"email"`
			} `json:"from"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(answer, &message); err != nil {
		return MailMessage{}, fmt.Errorf("mailstack GET /api/messages/%s: %w", id, err)
	}
	date, err := time.Parse(time.RFC3339, message.Meta.Date)
	if err != nil {
		return MailMessage{}, fmt.Errorf("mailstack message %s has date %q: %w", id, message.Meta.Date, err)
	}
	body := message.BodyText
	if message.BodyHTML != "" {
		body = pageText(message.BodyHTML)
	}
	return MailMessage{ID: message.Meta.ID, Subject: message.Meta.Subject, From: message.Meta.From.Email, Date: date, BodyText: body}, nil
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

// BudgetStore is budget-store's routes at URL, asked through HTTPClient: in
// the server, a transport that answers budget-store's in-process host as the
// owner on the request's context.
type BudgetStore struct {
	URL        string
	HTTPClient *http.Client
}

func (budget BudgetStore) send(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	if budget.URL == "" {
		return nil, 0, errors.New("budget-store's address is not set, so orders cannot be read or written")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, budget.URL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := budget.HTTPClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("budget-store %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("budget-store %s %s: read answer: %w", method, path, err)
	}
	if response.StatusCode/100 != 2 {
		err := fmt.Errorf("budget-store %s %s answered %d: %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
		if response.StatusCode == http.StatusConflict {
			err = fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return answer, response.StatusCode, err
	}
	return answer, response.StatusCode, nil
}

// ListOrders reads every marketplace order, a page of 1000 at a time.
func (budget BudgetStore) ListOrders(ctx context.Context) ([]BudgetOrder, error) {
	orders := []BudgetOrder{}
	for offset := 0; ; offset += 1000 {
		answer, _, err := budget.send(ctx, http.MethodGet, "/marketplace-orders?limit=1000&offset="+strconv.Itoa(offset), nil)
		if err != nil {
			return nil, err
		}
		var page []BudgetOrder
		if err := json.Unmarshal(answer, &page); err != nil {
			return nil, fmt.Errorf("budget-store GET /marketplace-orders: %w", err)
		}
		orders = append(orders, page...)
		if len(page) < 1000 {
			return orders, nil
		}
	}
}

// CreateOrder records an order; budget-store looks for its charge.
func (budget BudgetStore) CreateOrder(ctx context.Context, order BudgetOrder) (BudgetOrder, error) {
	answer, _, err := budget.send(ctx, http.MethodPost, "/marketplace-orders", order)
	if err != nil {
		return BudgetOrder{}, err
	}
	var created BudgetOrder
	if err := json.Unmarshal(answer, &created); err != nil {
		return BudgetOrder{}, fmt.Errorf("budget-store POST /marketplace-orders: %w", err)
	}
	return created, nil
}

// LinkPossession links the line to the possession and marks it one.
func (budget BudgetStore) LinkPossession(ctx context.Context, lineID, possessionID, note string) error {
	_, _, err := budget.send(ctx, http.MethodPost, "/links", map[string]string{"from_id": lineID, "entity_type": "possession", "entity_ref": possessionID, "note": note})
	if err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	_, _, err = budget.send(ctx, http.MethodPatch, "/marketplace-order-lines/"+url.PathEscape(lineID), map[string]string{"disposition": "possession"})
	return err
}

// OrderOfLine reads a line and its order; a line budget-store does not
// have is ErrInvalid.
func (budget BudgetStore) OrderOfLine(ctx context.Context, lineID string) (BudgetOrder, BudgetOrderLine, error) {
	answer, status, err := budget.send(ctx, http.MethodGet, "/marketplace-order-lines/"+url.PathEscape(lineID), nil)
	if status == http.StatusNotFound {
		return BudgetOrder{}, BudgetOrderLine{}, fmt.Errorf("%w: budget-store has no order line %s", ErrInvalid, lineID)
	}
	if err != nil {
		return BudgetOrder{}, BudgetOrderLine{}, err
	}
	var line BudgetOrderLine
	if err := json.Unmarshal(answer, &line); err != nil {
		return BudgetOrder{}, BudgetOrderLine{}, fmt.Errorf("budget-store GET /marketplace-order-lines/%s: %w", lineID, err)
	}
	answer, _, err = budget.send(ctx, http.MethodGet, "/marketplace-orders/"+url.PathEscape(line.OrderID), nil)
	if err != nil {
		return BudgetOrder{}, BudgetOrderLine{}, err
	}
	var order BudgetOrder
	if err := json.Unmarshal(answer, &order); err != nil {
		return BudgetOrder{}, BudgetOrderLine{}, fmt.Errorf("budget-store GET /marketplace-orders/%s: %w", line.OrderID, err)
	}
	return order, line, nil
}
