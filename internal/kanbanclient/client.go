// Package kanbanclient is a small HTTP client for the local kanban-store
// (default :8305). Used by autoworker (creates cards) and kanban-curator
// (closes them). Intentionally narrow — only the calls the scheduler binaries
// need; not a general-purpose SDK.
package kanbanclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client

	// Set by EnsureBoard for callers that want to reuse the resolved IDs.
	BoardID       string
	ColumnsByName map[string]string
}

// ServiceTokenHeader is where kanban-store reads the token an internal service
// presents. kanban-store gates every route, so a job that sends no token is
// answered 401 and does no work.
const ServiceTokenHeader = "X-Kanban-Store-Service-Token"

// ServiceTokenEnvironmentVariable holds that token for every binary in this
// repo; the scheduler unit carries it, so the jobs it runs inherit it. Each
// binary declares it in its settings (internal/config) and hands the value to
// New and StampRequestsToKanbanStore; this package reads no environment.
const ServiceTokenEnvironmentVariable = "KANBAN_STORE_SERVICE_TOKEN"

// serviceTokenTransport puts the token on every request this client makes.
// It sits in the transport rather than at each of the thirty-odd call sites
// below, so a route added later carries it too.
type serviceTokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *serviceTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// The caller's request must not be mutated; RoundTrip may be retried.
	cloned := request.Clone(request.Context())
	cloned.Header.Set(ServiceTokenHeader, t.token)
	return t.base.RoundTrip(cloned)
}

// New builds a client that authenticates as an internal service with
// serviceToken. An empty token is left off rather than sent blank, so the 401
// names the missing header instead of a wrong token.
func New(baseURL, serviceToken string) *Client {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if serviceToken != "" {
		httpClient.Transport = &serviceTokenTransport{token: serviceToken, base: http.DefaultTransport}
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    httpClient,
	}
}

// ============================ Boards & columns ============================

type Board struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// The board's settings: WHAT a job runs with. The scheduler job owns only
	// WHEN it runs. Each is absent on the wire when unset, and kanban-store has
	// already checked each id with its owner on write — principal-store,
	// llm-bridge-server's GET /agents and GET /instances/{id} — so a value here
	// resolved at the time it was set.
	//
	// ⚠️ The four default_* fields below are the board's OWN defaults, not what
	// a given card runs with: the board's tag rules override them per card.
	// A dispatcher spawning a session for a card reads GetCardEffectiveDefaults
	// for that card, never these fields directly.

	// DefaultPrincipalID is applied by kanban-store itself on card creation;
	// no scheduler binary needs to assign it.
	DefaultPrincipalID string `json:"default_principal_id,omitempty"`
	// DefaultAgentID is agent-store's NUMERIC id as llm-bridge-server's
	// GET /agents lists it — never the slug, which is renameable. A dispatcher
	// resolves it to the slug at spawn time because the bridge's session
	// agent_id field takes the slug.
	DefaultAgentID string `json:"default_agent_id,omitempty"`
	// DefaultInstanceID is the llm-bridge-server harness instance sessions
	// for this board's cards are spawned on.
	DefaultInstanceID string `json:"default_instance_id,omitempty"`
	// DefaultBundleID is bundle-store's numeric id, as a string ("6"). A
	// dispatcher sends it verbatim as `bundle_id` on llm-bridge-server's
	// POST /sessions, which checks it with bundle-store and provisions the
	// bundle's tools at spawn.
	DefaultBundleID string `json:"default_bundle_id,omitempty"`
	// Classifier is what email-classifier reads before filing mail onto this
	// board. Nil when the board has none set.
	Classifier *ClassifierConfig `json:"classifier,omitempty"`
}

// ClassifierConfig mirrors kanban-store's board `classifier` object.
type ClassifierConfig struct {
	// Vocabulary names one of email-classifier's classification schemes.
	// email-classifier owns the list (`email-classifier -list-vocabularies`)
	// and refuses a name it lacks.
	Vocabulary string `json:"vocabulary"`
	// MailAccountIDs are the mailstack account ids (GET /api/accounts → id)
	// the classifier reads for this board. Explicit, never "every account".
	MailAccountIDs []string `json:"mail_account_ids"`
	// HoldNewCards parks every card the classifier creates, so an unattended
	// worker discovering noteboard todos does not pick one up.
	HoldNewCards bool `json:"hold_new_cards"`
	// OrganizationID is the principal-store group the classifier's model
	// calls are made for, as llm-bridge-server operations. kanban-store
	// checks on write that it is an active group.
	OrganizationID string `json:"organization_id,omitempty"`
}

// ErrBoardNotFound is returned by GetBoard when kanban-store answers 404 for
// the id. Distinct so a caller can tell "the pinned id is gone" from "the
// store is down" — the first is a configuration fault, the second transient.
var ErrBoardNotFound = errors.New("kanban board not found")

// GetBoard fetches one board by id, settings included.
func (c *Client) GetBoard(id string) (*Board, error) {
	if id == "" {
		return nil, errors.New("kanbanclient: GetBoard called with an empty board id")
	}
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/boards/%s", c.BaseURL, id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: id %s", ErrBoardNotFound, id)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("get board "+id, resp)
	}
	var b Board
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ============================ Effective defaults ============================

// DefaultField names one of the four defaults a board or one of its tag rules
// may set. Its value is the JSON key both carry in kanban-store, and the key
// EffectiveDefaults.Defaults is indexed by.
type DefaultField string

const (
	DefaultFieldPrincipal DefaultField = "default_principal_id"
	DefaultFieldAgent     DefaultField = "default_agent_id"
	DefaultFieldInstance  DefaultField = "default_instance_id"
	DefaultFieldBundle    DefaultField = "default_bundle_id"
)

// The kinds of setting a resolved default can be read from, as kanban-store
// writes them in DefaultSource.Kind.
const (
	DefaultSourceKindBoard   = "board"
	DefaultSourceKindTagRule = "tag_rule"
)

// DefaultSource says which setting a resolved default was read from: the
// board's own default, or one tag rule — named by its id, with its tags and
// position carried for display.
type DefaultSource struct {
	Kind         string   `json:"kind"`
	RuleID       string   `json:"rule_id,omitempty"`
	RuleTags     []string `json:"rule_tags,omitempty"`
	RulePosition *int     `json:"rule_position,omitempty"`
}

// Describe renders the source for a log line: "board", or the tag rule's tags,
// position and id. A kind this client does not know is printed as sent rather
// than dropped.
func (s DefaultSource) Describe() string {
	switch s.Kind {
	case DefaultSourceKindBoard:
		return "board"
	case DefaultSourceKindTagRule:
		position := "?"
		if s.RulePosition != nil {
			position = fmt.Sprint(*s.RulePosition)
		}
		return fmt.Sprintf("tag rule [%s] (position %s, id %s)", strings.Join(s.RuleTags, " + "), position, s.RuleID)
	default:
		return fmt.Sprintf("source kind %q", s.Kind)
	}
}

// EffectiveDefault is one resolved default and where it came from.
type EffectiveDefault struct {
	Value  string        `json:"value"`
	Source DefaultSource `json:"source"`
}

// EffectiveDefaults is what one card gets on one board once kanban-store has
// applied the board's tag rules to the card's tags: for each default, the
// first matching rule that sets it, else the board's own value. The
// precedence lives in kanban-store only — this client reads the answer and
// never re-derives it.
type EffectiveDefaults struct {
	BoardID        string   `json:"board_id"`
	CardID         string   `json:"card_id"`
	Tags           []string `json:"tags"`
	MatchedRuleIDs []string `json:"matched_rule_ids"`
	// Defaults holds only the defaults that have a value somewhere. A field
	// absent here is set by no matching rule and not by the board either.
	Defaults map[DefaultField]EffectiveDefault `json:"defaults"`
}

// Default returns one resolved default, and false when nothing sets it.
func (e *EffectiveDefaults) Default(field DefaultField) (EffectiveDefault, bool) {
	resolved, ok := e.Defaults[field]
	return resolved, ok
}

// ErrCardNotFound is returned by GetCardEffectiveDefaults when kanban-store
// answers 404: noteboard has no item with that card id.
var ErrCardNotFound = errors.New("kanban card not found")

// GetCardEffectiveDefaults asks kanban-store which defaults one card gets on
// one board (GET /api/boards/{board}/cards/{card}/effective-defaults). A 404 is
// ErrCardNotFound; any other non-200 — 502 when kanban-store cannot read the
// card from noteboard — is an error carrying the body.
func (c *Client) GetCardEffectiveDefaults(boardID, cardID string) (*EffectiveDefaults, error) {
	if boardID == "" || cardID == "" {
		return nil, fmt.Errorf("kanbanclient: GetCardEffectiveDefaults needs a board id and a card id, got board %q card %q", boardID, cardID)
	}
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/boards/%s/cards/%s/effective-defaults", c.BaseURL, boardID, cardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: card %s on board %s — %s", ErrCardNotFound, cardID, boardID, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("effective defaults of card "+cardID+" on board "+boardID, resp)
	}
	var resolved EffectiveDefaults
	if err := json.NewDecoder(resp.Body).Decode(&resolved); err != nil {
		return nil, fmt.Errorf("decode effective defaults of card %s on board %s: %w", cardID, boardID, err)
	}
	// An answer about some other card or board would be applied to this one.
	if resolved.BoardID != boardID || resolved.CardID != cardID {
		return nil, fmt.Errorf("kanban effective defaults: asked for card %s on board %s, answered for card %q on board %q",
			cardID, boardID, resolved.CardID, resolved.BoardID)
	}
	return &resolved, nil
}

type Column struct {
	ID         string  `json:"id"`
	BoardID    string  `json:"board_id"`
	Name       string  `json:"name"`
	Position   float64 `json:"position"`
	AutoStatus string  `json:"auto_status,omitempty"`
}

// EnsureBoard finds (or creates) a board by name and ensures the named columns
// exist. Caches the resolved ids on the client. Idempotent.
func (c *Client) EnsureBoard(name string, wantColumns []string) error {
	board, err := c.findBoardByName(name)
	if err != nil {
		return err
	}
	if board == nil {
		board, err = c.createBoard(name)
		if err != nil {
			return fmt.Errorf("create board %q: %w", name, err)
		}
	}
	return c.ensureColumns(board.ID, wantColumns)
}

// EnsureBoardColumns is EnsureBoard for a caller that pins the board by id:
// it fetches the board (ErrBoardNotFound if the id is gone — never created,
// since creating a replacement is how a board forks), ensures the named
// columns exist, and caches the resolved ids on the client. Returns the board
// so the caller has its name and settings.
func (c *Client) EnsureBoardColumns(boardID string, wantColumns []string) (*Board, error) {
	board, err := c.GetBoard(boardID)
	if err != nil {
		return nil, err
	}
	if err := c.ensureColumns(board.ID, wantColumns); err != nil {
		return nil, err
	}
	return board, nil
}

func (c *Client) ensureColumns(boardID string, wantColumns []string) error {
	c.BoardID = boardID

	cols, err := c.ListColumns(boardID)
	if err != nil {
		return fmt.Errorf("list columns: %w", err)
	}
	have := map[string]string{}
	for _, col := range cols {
		have[col.Name] = col.ID
	}
	for _, want := range wantColumns {
		if _, ok := have[want]; ok {
			continue
		}
		var autoStatus *string
		switch want {
		case "Done":
			s := "done"
			autoStatus = &s
		case "Failed":
			s := "archived"
			autoStatus = &s
		}
		col, err := c.createColumn(boardID, want, autoStatus)
		if err != nil {
			return fmt.Errorf("create column %q: %w", want, err)
		}
		have[want] = col.ID
	}
	c.ColumnsByName = have
	return nil
}

// FindBoard returns the board with the given name, or (nil, nil) if not found.
func (c *Client) FindBoard(name string) (*Board, error) {
	return c.findBoardByName(name)
}

// ListBoards returns every board kanban-store knows about.
//
// Exported because resolving a board by name is unsafe for a writer that must
// keep hitting the same board: kanban-store has no unique constraint on board
// name and creates on miss, so a rename in the UI forks a second board and the
// writer silently follows the new one. A caller that pins a board id needs this
// to confirm the id still resolves.
func (c *Client) ListBoards() ([]Board, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/boards")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list boards", resp)
	}
	var boards []Board
	if err := json.NewDecoder(resp.Body).Decode(&boards); err != nil {
		return nil, err
	}
	return boards, nil
}

func (c *Client) findBoardByName(name string) (*Board, error) {
	boards, err := c.ListBoards()
	if err != nil {
		return nil, err
	}
	for i := range boards {
		if boards[i].Name == name {
			return &boards[i], nil
		}
	}
	return nil, nil
}

func (c *Client) createBoard(name string) (*Board, error) {
	body, _ := json.Marshal(map[string]any{
		"name":        name,
		"description": "Auto-tracked agent dispatches (autoworker, scheduler agent jobs).",
	})
	resp, err := c.HTTP.Post(c.BaseURL+"/api/boards", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, statusErr("create board", resp)
	}
	var b Board
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Client) ListColumns(boardID string) ([]Column, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/boards/%s/columns", c.BaseURL, boardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list columns", resp)
	}
	var cols []Column
	if err := json.NewDecoder(resp.Body).Decode(&cols); err != nil {
		return nil, err
	}
	return cols, nil
}

func (c *Client) createColumn(boardID, name string, autoStatus *string) (*Column, error) {
	payload := map[string]any{"name": name}
	if autoStatus != nil {
		payload["auto_status"] = *autoStatus
	}
	body, _ := json.Marshal(payload)
	resp, err := c.HTTP.Post(fmt.Sprintf("%s/api/boards/%s/columns", c.BaseURL, boardID),
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, statusErr("create column "+name, resp)
	}
	var col Column
	if err := json.NewDecoder(resp.Body).Decode(&col); err != nil {
		return nil, err
	}
	return &col, nil
}

// ============================ Cards ============================

// CardView mirrors kanban-store's BoardView entry: a placement plus the raw
// noteboard item. We treat the item as opaque map[string]any.
type CardView struct {
	Placement struct {
		CardID   string  `json:"card_id"`
		BoardID  string  `json:"board_id"`
		ColumnID string  `json:"column_id"`
		Position float64 `json:"position"`
	} `json:"placement"`
	Item map[string]any `json:"item"`
	// Links is the card's entity links. kanban-store has always sent these in
	// the board view; decoding them here saves a caller one ListCardLinks round
	// trip per card, which on a board whose links ARE the payload (email-
	// classifier attaches every message as a link) is the difference between
	// one request and hundreds.
	Links []CardLink `json:"links"`
	// Ticket is present when this card came from outside: who asked, and how
	// they reached us. Absent on an ordinary card. Decoded here so a caller
	// can tell a card that already has a requester from one that does not,
	// without a read per card.
	Ticket *Ticket `json:"ticket"`
}

// Ticket is kanban-store's ticket row. The lifecycle is deliberately not in
// it: a ticket's state is the column its card sits in.
type Ticket struct {
	CardID               string `json:"card_id"`
	RequesterPrincipalID string `json:"requester_principal_id"`
	Channel              string `json:"channel"`
}

// ColumnView holds a column and its cards (in placement order).
type ColumnView struct {
	Column Column     `json:"column"`
	Cards  []CardView `json:"cards"`
}

// BoardView is the assembled response from /api/boards/:id/cards.
type BoardView struct {
	Board   Board        `json:"board"`
	Columns []ColumnView `json:"columns"`
}

// GetBoardView returns the full board+columns+cards tree.
func (c *Client) GetBoardView(boardID string) (*BoardView, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/boards/%s/cards", c.BaseURL, boardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("board view", resp)
	}
	var bv BoardView
	if err := json.NewDecoder(resp.Body).Decode(&bv); err != nil {
		return nil, err
	}
	return &bv, nil
}

// CreateCardArgs is the input to CreateCard.
type CreateCardArgs struct {
	ColumnName string
	Title      string
	Body       string
	Tags       []string
	SessionID  string // becomes a card_link entity_type=session (canonical llm-bridge-server session_id)
	Provider   string // becomes a card_link entity_type=autoworker_provider
	// Priority and DueAt are noteboard item fields. Both are omitted from the
	// payload at their zero values, so callers that do not rank their cards
	// send exactly what they sent before.
	Priority int
	DueAt    *time.Time
	// Hold creates the card already parked, with HoldReason saying why.
	//
	// A hold is how noteboard says "this is not yours to pick up": held items
	// are hidden from the default listing, which is the listing an unattended
	// worker discovers work from. A caller that files cards a specific pipeline
	// owns needs this, or the cards look to the whole fleet like unclaimed
	// todos — and get worked by whoever finds them first.
	Hold       bool
	HoldReason string
}

// CreateCard creates a new card on the bound board+column and attaches the
// standard autoworker links. EnsureBoard must have been called first.
func (c *Client) CreateCard(a CreateCardArgs) (cardID string, err error) {
	if c.BoardID == "" {
		return "", errors.New("kanbanclient: EnsureBoard not called")
	}
	colID, ok := c.ColumnsByName[a.ColumnName]
	if !ok {
		return "", fmt.Errorf("column %q missing on board", a.ColumnName)
	}

	payload := map[string]any{
		"title":     a.Title,
		"body":      a.Body,
		"tags":      a.Tags,
		"column_id": colID,
	}
	if a.Priority != 0 {
		payload["priority"] = a.Priority
	}
	if a.DueAt != nil {
		payload["due_at"] = a.DueAt.Format(time.RFC3339)
	}
	if a.Hold {
		payload["hold"] = true
		payload["hold_reason"] = a.HoldReason
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/boards/%s/cards", c.BaseURL, c.BoardID)
	resp, err := c.HTTP.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", statusErr("create card", resp)
	}
	var created struct {
		Item map[string]any `json:"item"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	id, _ := created.Item["id"].(string)
	if id == "" {
		return "", errors.New("create card: empty item id in response")
	}

	type linkSpec struct{ entityType, entityRef, label string }
	links := []linkSpec{
		{"session", a.SessionID, "autoworker"},
		{"autoworker_provider", a.Provider, ""},
	}
	for _, l := range links {
		if l.entityRef == "" {
			continue
		}
		if err := c.AddCardLink(id, l.entityType, l.entityRef, l.label); err != nil {
			return id, fmt.Errorf("link %s/%s: %w", l.entityType, l.entityRef, err)
		}
	}
	return id, nil
}

// CardLink is one (card_id, entity_type, entity_ref) row.
type CardLink struct {
	ID         string `json:"id"`
	CardID     string `json:"card_id"`
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Label      string `json:"label"`
	// CreatedAt is when the link was attached, which for an email link is when
	// the classifier filed that message. kanban-store has always sent it; this
	// struct used to drop it on the floor, and a caller that wanted to ask "what
	// did we file since Tuesday" had no way to.
	CreatedAt time.Time `json:"created_at"`
}

// AddCardLink attaches a typed pointer to an external entity.
// PutTicket makes a card a ticket, or rewrites the one it has: who asked, how
// they reached us, and the record that made it a ticket. The requester is a
// principal-store contact id; kanban-store checks it and refuses a human or a
// group. The source is a registered entity type and ref — for mail, "email"
// and the mailstack locator — and kanban-store keeps the first one it is
// given: a different one later is a 409. Empty leaves any stored source alone.
//
// The lifecycle is deliberately not here. A ticket's state is the column its
// card sits in, so filing the card is what sets it.
func (c *Client) PutTicket(cardID, requesterPrincipalID, channel, sourceEntityType, sourceEntityRef string) error {
	body, _ := json.Marshal(map[string]string{
		"requester_principal_id": requesterPrincipalID,
		"channel":                channel,
		"source_entity_type":     sourceEntityType,
		"source_entity_ref":      sourceEntityRef,
	})
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/cards/%s/ticket", c.BaseURL, cardID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PUT ticket for card %s: %s — %s", cardID, resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (c *Client) AddCardLink(cardID, entityType, entityRef, label string) error {
	payload := map[string]any{"entity_type": entityType, "entity_ref": entityRef}
	if label != "" {
		payload["label"] = label
	}
	body, _ := json.Marshal(payload)
	resp, err := c.HTTP.Post(
		fmt.Sprintf("%s/api/cards/%s/links", c.BaseURL, cardID),
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return statusErr("create card link", resp)
	}
	return nil
}

// ListCardLinks returns all entity-link rows for a card.
func (c *Client) ListCardLinks(cardID string) ([]CardLink, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/cards/%s/links", c.BaseURL, cardID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list card links", resp)
	}
	var links []CardLink
	if err := json.NewDecoder(resp.Body).Decode(&links); err != nil {
		return nil, err
	}
	return links, nil
}

// MoveCard moves a card to a different column on the given board. position
// defaults to 0 (top); pass 0 if you don't care about relative ordering.
// If the destination column has auto_status set, the response will indicate
// whether the noteboard status patch succeeded.
func (c *Client) MoveCard(cardID, boardID, columnID string, position float64) error {
	payload := map[string]any{
		"board_id":  boardID,
		"column_id": columnID,
		"position":  position,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/cards/%s/move", c.BaseURL, cardID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("move card", resp)
	}
	return nil
}

// PatchCard updates fields on the noteboard item behind a card. kanban-store
// forwards the body verbatim to noteboard's PATCH /api/items/:id, so the
// accepted field names are noteboard's: title, body, tags, status, priority,
// due_at, parent_id, links.
//
// Callers should send only the fields they are changing. noteboard's revision
// snapshot covers title, body, tags, status, priority, list_id, parent_id and
// links — but NOT rank, due_at or schedule, so overwriting one of those three
// destroys the old value with no recoverable history.
func (c *Client) PatchCard(cardID string, fields map[string]any) error {
	payload, _ := json.Marshal(fields)
	req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/api/cards/%s", c.BaseURL, cardID), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("patch card", resp)
	}
	return nil
}

// PatchCardBody updates the noteboard item's body via the kanban-store proxy.
// Used by closure curator to append final-state notes.
func (c *Client) PatchCardBody(cardID, body string) error {
	return c.PatchCard(cardID, map[string]any{"body": body})
}

// HoldCard parks the card's work: the hold lands on the underlying noteboard
// item, so it withholds the work from every agent path — not just from whoever
// reads this board's columns.
func (c *Client) HoldCard(cardID, reason string) error {
	payload, _ := json.Marshal(map[string]any{"reason": reason})
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/cards/%s/hold", c.BaseURL, cardID), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("hold card", resp)
	}
	return nil
}

// ============================ Entity tags ============================

// EntityTag is one (entity_type, entity_ref, tag) row.
type EntityTag struct {
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Tag        string `json:"tag"`
}

// ListEntityTags returns all tags on a single (entity_type, entity_ref) pair.
func (c *Client) ListEntityTags(entityType, entityRef string) ([]EntityTag, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/entities/%s/%s/tags",
		c.BaseURL, entityType, entityRef))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list entity tags", resp)
	}
	var tags []EntityTag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// AddEntityTag attaches a tag to (entity_type, entity_ref). Idempotent.
func (c *Client) AddEntityTag(entityType, entityRef, tag string) error {
	body, _ := json.Marshal(map[string]any{"tag": tag})
	resp, err := c.HTTP.Post(
		fmt.Sprintf("%s/api/entities/%s/%s/tags", c.BaseURL, entityType, entityRef),
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return statusErr("add entity tag", resp)
	}
	return nil
}

// DeleteEntityTag removes a single tag from (entity_type, entity_ref).
func (c *Client) DeleteEntityTag(entityType, entityRef, tag string) error {
	req, _ := http.NewRequest("DELETE",
		fmt.Sprintf("%s/api/entities/%s/%s/tags/%s", c.BaseURL, entityType, entityRef, tag), nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("delete entity tag", resp)
	}
	return nil
}

// ============================ Reverse lookups ============================

// EntityCardEntry is one row from /api/entities/:type/:ref/cards.
type EntityCardEntry struct {
	CardID string         `json:"card_id"`
	Item   map[string]any `json:"item"`
}

// ListCardsForEntity returns the cards linked to (entity_type, entity_ref).
func (c *Client) ListCardsForEntity(entityType, entityRef string) ([]EntityCardEntry, error) {
	resp, err := c.HTTP.Get(fmt.Sprintf("%s/api/entities/%s/%s/cards",
		c.BaseURL, entityType, entityRef))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("list entity cards", resp)
	}
	var entries []EntityCardEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// ============================ helpers ============================

func statusErr(op string, resp *http.Response) error {
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("kanban %s: %s — %s", op, resp.Status, strings.TrimSpace(string(raw)))
}

// DeleteCardLink removes one entity link by its link id.
//
// Needed by the merge pass, which must MOVE a card's evidence rather than copy
// it: a link left on a card that has been taken off the board would leave its
// email attached to nothing visible, and the filing pass — which reads the board
// to learn what is already filed — would then see that email as unfiled and file
// it a second time.
func (c *Client) DeleteCardLink(linkID string) error {
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/links/%s", c.BaseURL, linkID), nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("delete card link", resp)
	}
	return nil
}

// DetachCardFromBoard removes a card's placement from one board, leaving the
// underlying noteboard item intact.
//
// Distinct from deleting the card: the item is what a human may have edited, and
// taking a card off a board is a presentation decision, not a decision to
// destroy the work it describes.
func (c *Client) DetachCardFromBoard(boardID, cardID string) error {
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/boards/%s/cards/%s", c.BaseURL, boardID, cardID), nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr("detach card from board", resp)
	}
	return nil
}

// hostScopedServiceTokenTransport stamps the token only on requests to
// kanban-store's host. A binary that calls kanban-store with its own
// http.Client, rather than through Client above, also calls noteboard,
// llm-bridge-server and mailstack through the same default transport, and
// kanban-store's token has no business being sent to any of them.
type hostScopedServiceTokenTransport struct {
	host  string
	token string
	base  http.RoundTripper
}

func (t *hostScopedServiceTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !strings.EqualFold(request.URL.Host, t.host) {
		return t.base.RoundTrip(request)
	}
	cloned := request.Clone(request.Context())
	cloned.Header.Set(ServiceTokenHeader, t.token)
	return t.base.RoundTrip(cloned)
}

// StampRequestsToKanbanStore installs that transport as the process-wide
// default, so every call to kanban-store carries the token however it was
// made: http.Get, http.DefaultClient, or any client that left Transport nil.
// It is the mirror of bridgeauth.StampRequestsToBridge and composes with it;
// call each once, early in main, with the URL the binary will use.
//
// demo-worker needed it: it writes cards with its own client — attributed
// links, notes, unhold — which the thirty-odd methods of Client do not cover,
// and until 2026-09-18 every one of those calls was answered 401 once
// kanban-store began gating its routes.
//
// An unset token is not an error: the binary is then talking to a
// kanban-store that does not gate, and a 401 names the missing header if it
// does.
func StampRequestsToKanbanStore(kanbanURL, serviceToken string) error {
	if serviceToken == "" {
		return nil
	}
	parsed, err := url.Parse(kanbanURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("kanbanclient: %q is not a URL with a host, so requests to kanban-store cannot be authenticated: %v", kanbanURL, err)
	}
	http.DefaultTransport = &hostScopedServiceTokenTransport{host: parsed.Host, token: serviceToken, base: http.DefaultTransport}
	http.DefaultClient.Transport = http.DefaultTransport
	return nil
}
