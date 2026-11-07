package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

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

// ArticleStore is article-store over HTTP.
type ArticleStore struct {
	URL        string
	HTTPClient *http.Client
}

// SaveListing saves the page (article-store fetches it; a URL already saved
// answers the stored article) and answers its article id.
func (store ArticleStore) SaveListing(ctx context.Context, pageURL string, tags []string, addedBy string) (string, error) {
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

// Mailstack is mailstack over HTTP with its bearer token.
type Mailstack struct {
	URL        string
	Token      string
	HTTPClient *http.Client
}

func (mail Mailstack) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if mail.Token == "" {
		return nil, fmt.Errorf("MAILSTACK_TOKEN is not set, so mail cannot be read")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, mail.URL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+mail.Token)
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
