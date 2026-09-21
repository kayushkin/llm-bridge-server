// Package kanbanclient is a minimal HTTP client for kanban-store. bridge-server
// uses it to answer which noteboard todo a session is linked to, to read that
// card's text and links when it triages a worker's question, and to record on
// the card's timeline that the card is waiting on an answer.
//
// kanban-store owns that link. A card id in kanban-store IS a noteboard item
// id — cards have no rows of their own, only placements and links against the
// noteboard item — so the card ids it returns for a session are todo ids
// directly, with no second join and no name matching.
package kanbanclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is a kanban-store reverse-lookup caller. Safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
}

// New constructs a Client. baseURL is the kanban-store root (e.g.
// "http://localhost:8305").
// ServiceTokenHeader is where kanban-store reads an internal service's token,
// and ServiceTokenEnvironmentVariable is where this server's unit carries it.
// kanban-store gates every route: without the token every lookup here is a 401,
// and a signal is minted with no linked todo.
const (
	ServiceTokenHeader              = "X-Kanban-Store-Service-Token"
	ServiceTokenEnvironmentVariable = "KANBAN_STORE_SERVICE_TOKEN"
)

// serviceTokenTransport puts the token on every request this client makes, so a
// lookup added later carries it too.
type serviceTokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *serviceTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set(ServiceTokenHeader, t.token)
	return t.base.RoundTrip(cloned)
}

// New builds a client that authenticates as an internal service with the token
// in KANBAN_STORE_SERVICE_TOKEN. An empty token is left off rather than sent
// blank, so kanban-store's 401 names the missing header.
func New(baseURL string) *Client {
	client := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Loopback HTTP against a SQLite reverse index — typically
		// sub-millisecond. The timeout is a ceiling so a wedged store fails
		// the lookup quickly instead of holding up whatever raised the
		// signal.
		http: &http.Client{Timeout: 3 * time.Second},
	}
	if token := os.Getenv(ServiceTokenEnvironmentVariable); token != "" {
		client.http.Transport = &serviceTokenTransport{token: token, base: http.DefaultTransport}
	}
	return client
}

// entityCard is one row of GET /api/entities/{type}/{ref}/cards. The route
// returns parallel card_id/item entries so a caller can spot an orphan — a
// link whose noteboard item is gone, which arrives as a non-empty card_id
// with a null item.
type entityCard struct {
	CardID string          `json:"card_id"`
	Item   json.RawMessage `json:"item"`
}

// LinkedCard is the card a session is linked to, with the text of the work it
// carries. A card id IS a noteboard item id, and Title and Body are that
// item's — read here off the reverse lookup rather than fetched again.
type LinkedCard struct {
	ID    string
	Title string
	Body  string
}

// CardLink is one link on a card, as kanban-store returns it. EntityType is
// open on the store side; the two this client reads are "email" (ref
// "<account_id>:<provider message id>") and "email_sender" (a lowercased
// address).
type CardLink struct {
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Label      string `json:"label"`
}

// LinkedTodoForSession returns the noteboard todo id linked to this session,
// or "" when the session has no link.
//
// A session can be linked to several cards — the kanban classifier mints one
// per distinct piece of work it recognizes in a session — so this picks the
// oldest link. A session's reason for existing is recorded before it does
// anything: the autoworker links its dispatch todo at fire time, and every
// later link describes work the session has since done. The oldest link is
// therefore the todo the session is *for*, and the ordering that makes it
// reachable is kanban-store's, not a re-sort here.
//
// An orphaned link is skipped rather than returned. Its card id would still
// resolve to nothing in a todo view, and pointing a signal at a deleted item
// is worse than leaving it unlinked.
func (c *Client) LinkedTodoForSession(ctx context.Context, sessionID string) (string, error) {
	card, err := c.LinkedCardForSession(ctx, sessionID)
	if err != nil || card == nil {
		return "", err
	}
	return card.ID, nil
}

// LinkedCardForSession returns the card this session is linked to, with its
// title and body, or nil when there is none.
func (c *Client) LinkedCardForSession(ctx context.Context, sessionID string) (*LinkedCard, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	endpoint := fmt.Sprintf("%s/api/entities/session/%s/cards", c.baseURL, url.PathEscape(sessionID))
	body, err := c.get(ctx, endpoint, "linked-todo lookup")
	if err != nil {
		return nil, err
	}
	var cards []entityCard
	if err := json.Unmarshal(body, &cards); err != nil {
		return nil, fmt.Errorf("parse linked-todo response: %w", err)
	}
	for _, card := range cards {
		if card.CardID == "" {
			continue
		}
		if len(card.Item) == 0 || string(card.Item) == "null" {
			continue
		}
		var item struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.Unmarshal(card.Item, &item); err != nil {
			return nil, fmt.Errorf("parse linked card %s item: %w", card.CardID, err)
		}
		return &LinkedCard{ID: card.CardID, Title: item.Title, Body: item.Body}, nil
	}
	return nil, nil
}

// CardLinks returns every link on a card.
func (c *Client) CardLinks(ctx context.Context, cardID string) ([]CardLink, error) {
	if cardID == "" {
		return nil, fmt.Errorf("card id is required")
	}
	endpoint := fmt.Sprintf("%s/api/cards/%s/links", c.baseURL, url.PathEscape(cardID))
	body, err := c.get(ctx, endpoint, "card links")
	if err != nil {
		return nil, err
	}
	var links []CardLink
	if err := json.Unmarshal(body, &links); err != nil {
		return nil, fmt.Errorf("parse card links response: %w", err)
	}
	return links, nil
}

// CardEvent is an action to record on a card's timeline. Kind is one of
// kanban-store's event kinds ("waiting_started", "waiting_ended", …); the
// store supplies the clock state those kinds imply, so none is sent here.
type CardEvent struct {
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// PostCardEvent records an action on a card's timeline.
func (c *Client) PostCardEvent(ctx context.Context, cardID string, event CardEvent) error {
	if cardID == "" {
		return fmt.Errorf("card id is required")
	}
	if event.Kind == "" {
		return fmt.Errorf("event kind is required")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode card event: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/cards/%s/events", c.baseURL, url.PathEscape(cardID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("build card event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kanban-store card event: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("kanban-store card event: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// get performs one GET and returns the body of a 200, or an error naming
// what was being looked up.
func (c *Client) get(ctx context.Context, endpoint, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", what, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kanban-store %s: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", what, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: kanban-store %s: status 404: %s", ErrNotFound, what, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kanban-store %s: status %d: %s", what, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// ErrNotFound is kanban-store answering 404: the card or board is not one it
// has. Any other failure is the store not answering, and is not this error.
var ErrNotFound = errors.New("not found")

// CardTags returns the tags of one card — its noteboard item's tags, which
// kanban-store passes through on GET /api/cards/{id}. A card kanban-store
// does not have, or whose noteboard item is gone, is ErrNotFound: either way
// there are no tags to read, and an empty list would pass for "no tags".
func (c *Client) CardTags(ctx context.Context, cardID string) ([]string, error) {
	if cardID == "" {
		return nil, fmt.Errorf("card id is required")
	}
	body, err := c.get(ctx, fmt.Sprintf("%s/api/cards/%s", c.baseURL, url.PathEscape(cardID)), "card read")
	if err != nil {
		return nil, err
	}
	var card struct {
		CardID string `json:"card_id"`
		Item   *struct {
			Tags []string `json:"tags"`
		} `json:"item"`
	}
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("parse card %s: %w", cardID, err)
	}
	if card.Item == nil {
		return nil, fmt.Errorf("%w: card %s has no noteboard item", ErrNotFound, cardID)
	}
	if card.Item.Tags == nil {
		return []string{}, nil
	}
	return card.Item.Tags, nil
}

// EffectiveDefault is one board default as kanban-store resolved it for a
// card or a tag list, with where it came from: the board itself or a tag rule.
type EffectiveDefault struct {
	Value  string `json:"value"`
	Source struct {
		Kind         string   `json:"kind"`
		RuleID       string   `json:"rule_id,omitempty"`
		RuleTags     []string `json:"rule_tags,omitempty"`
		RulePosition *int     `json:"rule_position,omitempty"`
	} `json:"source"`
}

// EffectiveDefaults is kanban-store's answer to "what does a card on this
// board get": each of the four defaults that anything sets, with its source.
// A default absent from Defaults is set nowhere. The precedence lives in
// kanban-store alone; this is a read of its answer, never a re-resolution.
type EffectiveDefaults struct {
	BoardID        string                      `json:"board_id"`
	CardID         string                      `json:"card_id,omitempty"`
	Tags           []string                    `json:"tags"`
	MatchedRuleIDs []string                    `json:"matched_rule_ids"`
	Defaults       map[string]EffectiveDefault `json:"defaults"`
}

// EffectiveDefaults reads a board's resolved defaults: for a card when cardID
// is given, else for a card that would carry tags.
func (c *Client) EffectiveDefaults(ctx context.Context, boardID, cardID string, tags []string) (*EffectiveDefaults, error) {
	if boardID == "" {
		return nil, fmt.Errorf("board id is required")
	}
	var endpoint string
	if cardID != "" {
		endpoint = fmt.Sprintf("%s/api/boards/%s/cards/%s/effective-defaults", c.baseURL, url.PathEscape(boardID), url.PathEscape(cardID))
	} else {
		query := url.Values{}
		for _, tag := range tags {
			query.Add("tag", tag)
		}
		endpoint = fmt.Sprintf("%s/api/boards/%s/effective-defaults", c.baseURL, url.PathEscape(boardID))
		if len(query) > 0 {
			endpoint += "?" + query.Encode()
		}
	}
	body, err := c.get(ctx, endpoint, "effective defaults")
	if err != nil {
		return nil, err
	}
	var out EffectiveDefaults
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse effective-defaults response: %w", err)
	}
	return &out, nil
}
