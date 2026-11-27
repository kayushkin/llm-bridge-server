// The issues package (kanban-store) as this server reads and writes it: which
// noteboard todo a session is linked to, reads that card's text
// and links when it triages a worker's question, records on the card's
// timeline that the card is waiting on an answer, and reads a board's
// defaults and classification.
//
// kanban-store runs in this process (internal/tasktracking/issues), and
// every call here is a Go call to its API (issueStore), the same code its routes
// run, held to the same rules. A call is made as the internal service,
// unrestricted, unless it names a principal, and then the store's board
// grants decide.
//
// kanban-store owns that link. A card id in kanban-store IS a noteboard item
// id — cards have no rows of their own, only placements and links against the
// noteboard item — so the card ids it returns for a session are todo ids
// directly, with no second join and no name matching.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/executors"
	issuesapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/api"
	issuesmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// issueStore is kanban-store's Go surface as this client calls it:
// *issuesapi.API in the server, a fake in tests. Every method refuses as its
// route does, with a *issuesapi.Refusal carrying the route's status.
type issueStore interface {
	EntityCards(caller issuesapi.Caller, entityType, entityRef string) ([]issuesmodel.EntityCardView, error)
	CardLinks(caller issuesapi.Caller, cardID string) ([]issuesmodel.CardLink, error)
	RecordCardEvent(caller issuesapi.Caller, cardID string, request issuesmodel.CreateCardEventRequest) (*issuesmodel.CardEvent, error)
	CardTags(caller issuesapi.Caller, cardID string) ([]string, error)
	BoardEffectiveDefaults(caller issuesapi.Caller, boardID string, tags []string) (*issuesmodel.EffectiveDefaults, error)
	CardEffectiveDefaults(caller issuesapi.Caller, boardID, cardID string) (*issuesmodel.EffectiveDefaults, error)
	BoardClassification(caller issuesapi.Caller, boardID string) (*msg.BoardClassification, error)
	BoardCardPage(caller issuesapi.Caller, boardID string, query url.Values) (*issuesmodel.BoardCardPage, error)
	PublishClassificationDecision(caller issuesapi.Caller, publication msg.BoardClassificationDecisionPublication) (*issuesmodel.ClassificationDecision, bool, error)
}

// issueReader is kanban-store in this process. Safe for concurrent use.
type issueReader struct {
	store issueStore
}

// newIssueReader builds a client of store.
func newIssueReader(store issueStore) *issueReader {
	if store == nil {
		panic("newIssueReader needs kanban-store")
	}
	return &issueReader{store: store}
}

// issuesInternalService is the caller of every read this server makes for itself.
var issuesInternalService = issuesapi.Caller{InternalService: true}

// errIssueNotFound is kanban-store answering 404: the card or board is not one it
// has. Any other failure is the store not answering, and is not this error.
var errIssueNotFound = errors.New("not found")

// issueReadFailure is a refusal of a call made as the internal service: a 404 is
// errIssueNotFound, and every other status is named in the message.
func issueReadFailure(what string, err error) error {
	var refusal *issuesapi.Refusal
	if errors.As(err, &refusal) {
		if refusal.Status == http.StatusNotFound {
			return fmt.Errorf("%w: kanban-store %s: status 404: %s", errIssueNotFound, what, refusal.Message)
		}
		return fmt.Errorf("kanban-store %s: status %d: %s", what, refusal.Status, refusal.Message)
	}
	return fmt.Errorf("kanban-store %s: %w", what, err)
}

// issueReadFailureAsPrincipal is a refusal of a call made as a principal: a board it
// may not view (403 or 404) is errIssueNotFound, the same as a missing one.
func issueReadFailureAsPrincipal(what string, err error) error {
	var refusal *issuesapi.Refusal
	if errors.As(err, &refusal) && (refusal.Status == http.StatusNotFound || refusal.Status == http.StatusForbidden) {
		return fmt.Errorf("%w: kanban-store %s: status %d: %s", errIssueNotFound, what, refusal.Status, refusal.Message)
	}
	return issueReadFailure(what, err)
}

// issuesCallerAs is the caller a read names: the principal, whose board grants
// decide, or the internal service when it names none.
func issuesCallerAs(principalID string) issuesapi.Caller {
	if principalID == "" {
		return issuesInternalService
	}
	return issuesapi.Caller{PrincipalID: principalID}
}

// linkedIssueCard is the card a session is linked to, with the text of the work it
// carries. A card id IS a noteboard item id, and Title and Body are that
// item's — read here off the reverse lookup rather than fetched again.
type linkedIssueCard struct {
	ID    string
	Title string
	Body  string
}

// issueCardLink is one link on a card, as kanban-store returns it. EntityType is
// open on the store side; the two this client reads are "email" (ref
// "<account_id>:<provider message id>") and "email_sender" (a lowercased
// address).
type issueCardLink struct {
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
func (c *issueReader) LinkedTodoForSession(ctx context.Context, sessionID string) (string, error) {
	card, err := c.LinkedCardForSession(ctx, sessionID)
	if err != nil || card == nil {
		return "", err
	}
	return card.ID, nil
}

// LinkedCardForSession returns the card this session is linked to, with its
// title and body, or nil when there is none.
func (c *issueReader) LinkedCardForSession(_ context.Context, sessionID string) (*linkedIssueCard, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	cards, err := c.store.EntityCards(issuesInternalService, "session", sessionID)
	if err != nil {
		return nil, issueReadFailure("linked-todo lookup", err)
	}
	for _, card := range cards {
		if card.CardID == "" {
			continue
		}
		issueItem, isItem := card.Item.(map[string]any)
		if !isItem || issueItem == nil {
			// A link that outlived its noteboard item: an orphan.
			continue
		}
		title, err := issueItemTextField(issueItem, "title")
		if err != nil {
			return nil, fmt.Errorf("linked card %s item: %w", card.CardID, err)
		}
		body, err := issueItemTextField(issueItem, "body")
		if err != nil {
			return nil, fmt.Errorf("linked card %s item: %w", card.CardID, err)
		}
		return &linkedIssueCard{ID: card.CardID, Title: title, Body: body}, nil
	}
	return nil, nil
}

// issueItemTextField is a noteboard item's text field, "" when it is absent.
func issueItemTextField(issueItem map[string]any, field string) (string, error) {
	value, present := issueItem[field]
	if !present || value == nil {
		return "", nil
	}
	text, isText := value.(string)
	if !isText {
		return "", fmt.Errorf("%s is %T, not text", field, value)
	}
	return text, nil
}

// CardLinks returns every link on a card.
func (c *issueReader) CardLinks(_ context.Context, cardID string) ([]issueCardLink, error) {
	if cardID == "" {
		return nil, fmt.Errorf("card id is required")
	}
	stored, err := c.store.CardLinks(issuesInternalService, cardID)
	if err != nil {
		return nil, issueReadFailure("card links", err)
	}
	links := make([]issueCardLink, len(stored))
	for i, link := range stored {
		links[i] = issueCardLink{EntityType: link.EntityType, EntityRef: link.EntityRef, Label: link.Label}
	}
	return links, nil
}

// issueCardEvent is an action to record on a card's timeline. Kind is one of
// kanban-store's event kinds ("waiting_started", "waiting_ended", …); the
// store supplies the clock state those kinds imply, so none is sent here.
type issueCardEvent struct {
	Kind    string
	Actor   string
	Summary string
	Detail  json.RawMessage
}

// PostCardEvent records an action on a card's timeline.
func (c *issueReader) PostCardEvent(_ context.Context, cardID string, event issueCardEvent) error {
	if cardID == "" {
		return fmt.Errorf("card id is required")
	}
	if event.Kind == "" {
		return fmt.Errorf("event kind is required")
	}
	_, err := c.store.RecordCardEvent(issuesInternalService, cardID, issuesmodel.CreateCardEventRequest{
		Kind: issuesmodel.EventKind(event.Kind), Actor: event.Actor, Summary: event.Summary, Detail: event.Detail,
	})
	if err != nil {
		return issueReadFailure("card event", err)
	}
	return nil
}

// CardTags returns the tags of one card — its noteboard item's tags. A card
// kanban-store does not have, or whose noteboard item is gone, is
// errIssueNotFound: either way there are no tags to read, and an empty list would
// pass for "no tags".
func (c *issueReader) CardTags(_ context.Context, cardID string) ([]string, error) {
	if cardID == "" {
		return nil, fmt.Errorf("card id is required")
	}
	tags, err := c.store.CardTags(issuesInternalService, cardID)
	if err != nil {
		return nil, issueReadFailure("card read", err)
	}
	return tags, nil
}

// EffectiveDefaults is kanban-store's answer to "what does a card on this
// board get": each of the four defaults that anything sets, with its source.
// A default absent from Defaults is set nowhere. The precedence lives in
// kanban-store alone; this is its answer, never a re-resolution.
type EffectiveDefaults = issuesmodel.EffectiveDefaults

// EffectiveDefaults reads a board's resolved defaults: for a card when cardID
// is given, else for a card that would carry tags.
func (c *issueReader) EffectiveDefaults(_ context.Context, boardID, cardID string, tags []string) (*EffectiveDefaults, error) {
	if boardID == "" {
		return nil, fmt.Errorf("board id is required")
	}
	var resolved *issuesmodel.EffectiveDefaults
	var err error
	if cardID != "" {
		resolved, err = c.store.CardEffectiveDefaults(issuesInternalService, boardID, cardID)
	} else {
		resolved, err = c.store.BoardEffectiveDefaults(issuesInternalService, boardID, tags)
	}
	if err != nil {
		return nil, issueReadFailure("effective defaults", err)
	}
	return resolved, nil
}

// BoardTaxonomy reads the classification taxonomy a board keeps, archived
// entries included, and its taxonomy revision as the version it was read at.
// The revision moves only when the taxonomy does, not when anything else on
// the board is edited. With a principalID the read is made as that
// principal, so kanban-store's can_view decides: a board they may not view is
// errIssueNotFound, the same as a missing one. With none it is made as this
// service.
func (c *issueReader) BoardTaxonomy(_ context.Context, boardID, principalID string) (msg.ClassificationTaxonomy, string, error) {
	what := "read board " + boardID + " classification"
	if principalID != "" {
		what += " as " + principalID
	}
	classification, err := c.store.BoardClassification(issuesCallerAs(principalID), boardID)
	if err != nil {
		return msg.ClassificationTaxonomy{}, "", issueReadFailureAsPrincipal(what, err)
	}
	if classification.BoardID != boardID {
		return msg.ClassificationTaxonomy{}, "", fmt.Errorf("kanban-store answered board %s with board %q", boardID, classification.BoardID)
	}
	if classification.Taxonomy == nil {
		return msg.ClassificationTaxonomy{}, "", fmt.Errorf("%w: board %s", executors.ErrBoardHasNoTaxonomy, boardID)
	}
	if err := classification.Taxonomy.Validate(); err != nil {
		return msg.ClassificationTaxonomy{}, "", fmt.Errorf("board %s keeps a taxonomy that does not validate: %w", boardID, err)
	}
	return *classification.Taxonomy, strconv.FormatInt(classification.TaxonomyRevision, 10), nil
}
