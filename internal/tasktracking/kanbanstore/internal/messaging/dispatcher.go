// Package messaging fires a board's message triggers when a card event is
// recorded: it matches the event against the board's enabled triggers,
// renders each matching template, records a delivery row and hands the text
// to multichat.
package messaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/kanban-store/internal/db"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/kanban-store/internal/multichat"
	"github.com/kayushkin/kanban-store/internal/noteboard"
)

// Sender delivers one rendered message. *multichat.Client is the real one.
type Sender interface {
	SendDirectMessage(ctx context.Context, recipientUserID, body string) (multichat.SendResult, error)
}

type Dispatcher struct {
	store       *db.Store
	noteboard   *noteboard.Client
	sendTimeout time.Duration

	mu     sync.RWMutex
	sender Sender
}

// NewDispatcher starts with no sender: every matching trigger records a
// not_configured delivery until SetSender is called.
func NewDispatcher(store *db.Store, nb *noteboard.Client) *Dispatcher {
	return &Dispatcher{store: store, noteboard: nb, sendTimeout: 60 * time.Second}
}

func (d *Dispatcher) SetSender(sender Sender) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sender = sender
}

// DeliveryConfigured reports whether a sender is set.
func (d *Dispatcher) DeliveryConfigured() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sender != nil
}

func (d *Dispatcher) currentSender() Sender {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sender
}

// FireTriggersFor runs every matching trigger for one stored event, and
// returns when each has a delivery row. It blocks on multichat; the API calls
// it in a goroutine so a card write never waits on a message.
//
// An event with a board (created, moved) is matched against that board's
// triggers. A card-wide event (completed, assigned) is matched against the
// triggers of every board the card sits on.
func (d *Dispatcher) FireTriggersFor(event model.CardEvent) {
	if !model.IsMessageTriggerEventKind(event.Kind) {
		return
	}
	boardIDs, err := d.boardsOf(event)
	if err != nil {
		log.Printf("message triggers: event %s (%s on card %s): find boards: %v", event.ID, event.Kind, event.CardID, err)
		return
	}
	var card *cardFacts
	var cardErr error
	cardLoaded := false
	for _, boardID := range boardIDs {
		triggers, err := d.store.ListMessageTriggers(boardID, true)
		if err != nil {
			log.Printf("message triggers: event %s: list triggers for board %s: %v", event.ID, boardID, err)
			continue
		}
		var candidates []*model.MessageTrigger
		for _, trigger := range triggers {
			if trigger.EventKind != event.Kind {
				continue
			}
			if trigger.ToColumnID != "" && trigger.ToColumnID != event.ToColumnID {
				continue
			}
			candidates = append(candidates, trigger)
		}
		if len(candidates) == 0 {
			continue
		}
		if !cardLoaded {
			card, cardErr = d.loadCard(event.CardID)
			cardLoaded = true
		}
		board, boardErr := d.loadBoard(boardID, event)
		for _, trigger := range candidates {
			// Without the card or the board the priority filter cannot be
			// judged and the template cannot be filled, so the trigger is
			// recorded as failed rather than guessed at or skipped quietly.
			if loadErr := errors.Join(cardErr, boardErr); loadErr != nil {
				d.recordUnsent(trigger, event, boardID, "", model.DeliveryFailed, "load card event context: "+loadErr.Error())
				continue
			}
			priorityLevel := board.ladder.LevelFor(card.priorityValue)
			if trigger.PriorityValue != nil && (priorityLevel == nil || priorityLevel.PriorityValue != *trigger.PriorityValue) {
				continue
			}
			priorityLabel := ""
			if priorityLevel != nil {
				priorityLabel = priorityLevel.Label
			}
			d.deliver(trigger, event, boardID, model.MessageTemplateFields{
				CardID:         event.CardID,
				Title:          card.title,
				BoardName:      board.name,
				ColumnName:     board.columnName,
				FromColumnName: board.fromColumnName,
				PriorityLabel:  priorityLabel,
				EventKind:      string(event.Kind),
				Actor:          event.Actor,
				Summary:        event.Summary,
				AssigneeIDs:    card.assigneeIDs,
				OccurredAt:     event.OccurredAt,
			})
		}
	}
}

func (d *Dispatcher) boardsOf(event model.CardEvent) ([]string, error) {
	if event.BoardID != "" {
		return []string{event.BoardID}, nil
	}
	placements, err := d.store.ListPlacementsByCard(event.CardID)
	if err != nil {
		return nil, err
	}
	boardIDs := make([]string, 0, len(placements))
	for _, placement := range placements {
		boardIDs = append(boardIDs, placement.BoardID)
	}
	return boardIDs, nil
}

type cardFacts struct {
	title         string
	priorityValue int
	assigneeIDs   []string
}

func (d *Dispatcher) loadCard(cardID string) (*cardFacts, error) {
	item, err := d.noteboard.GetItem(cardID)
	if err != nil {
		return nil, fmt.Errorf("noteboard item %s: %w", cardID, err)
	}
	title, _ := item["title"].(string)
	assignments, err := d.store.ListCardAssignments(cardID)
	if err != nil {
		return nil, fmt.Errorf("assignments of %s: %w", cardID, err)
	}
	assigneeIDs := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		assigneeIDs = append(assigneeIDs, assignment.PrincipalID)
	}
	return &cardFacts{title: title, priorityValue: model.PriorityOfNoteboardItem(item), assigneeIDs: assigneeIDs}, nil
}

type boardFacts struct {
	name           string
	ladder         *model.PriorityLadder
	columnName     string
	fromColumnName string
}

func (d *Dispatcher) loadBoard(boardID string, event model.CardEvent) (*boardFacts, error) {
	board, err := d.store.GetBoard(boardID)
	if err != nil {
		return nil, fmt.Errorf("board %s: %w", boardID, err)
	}
	ladder, err := d.store.GetPriorityLadder(boardID)
	if err != nil {
		return nil, fmt.Errorf("priority ladder of %s: %w", boardID, err)
	}
	facts := &boardFacts{name: board.Name, ladder: ladder}
	columnID := event.ToColumnID
	if columnID == "" {
		placement, err := d.store.GetPlacement(event.CardID, boardID)
		if err != nil {
			return nil, fmt.Errorf("placement of %s on %s: %w", event.CardID, boardID, err)
		}
		columnID = placement.ColumnID
	}
	column, err := d.store.GetColumn(columnID)
	if err != nil {
		return nil, fmt.Errorf("column %s: %w", columnID, err)
	}
	facts.columnName = column.Name
	if event.FromColumnID != "" {
		from, err := d.store.GetColumn(event.FromColumnID)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", event.FromColumnID, err)
		}
		facts.fromColumnName = from.Name
	}
	return facts, nil
}

func (d *Dispatcher) deliver(trigger *model.MessageTrigger, event model.CardEvent, boardID string, fields model.MessageTemplateFields) {
	template, err := model.ParseMessageTemplate(trigger.MessageTemplate)
	var rendered bytes.Buffer
	if err == nil {
		err = template.Execute(&rendered, fields)
	}
	if err != nil {
		d.recordUnsent(trigger, event, boardID, "", model.DeliveryFailed, "render message_template: "+err.Error())
		return
	}
	message := rendered.String()
	if strings.TrimSpace(message) == "" {
		d.recordUnsent(trigger, event, boardID, message, model.DeliveryFailed, "message_template rendered to an empty message")
		return
	}
	sender := d.currentSender()
	if sender == nil {
		d.recordUnsent(trigger, event, boardID, message, model.DeliveryNotConfigured, "MULTICHAT_URL is not set on kanban-store; nothing was sent")
		return
	}
	delivery := &model.MessageDelivery{
		TriggerID: trigger.ID, BoardID: boardID, EventID: event.ID, CardID: event.CardID,
		RecipientUserID: trigger.RecipientUserID, RenderedMessage: message, Status: model.DeliveryPending,
	}
	if !d.claim(delivery) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.sendTimeout)
	defer cancel()
	result, err := sender.SendDirectMessage(ctx, trigger.RecipientUserID, message)
	if err != nil {
		log.Printf("message triggers: trigger %s %q on event %s: send to %s failed: %v", trigger.ID, trigger.Name, event.ID, trigger.RecipientUserID, err)
		if finishErr := d.store.FinishMessageDelivery(delivery.ID, model.DeliveryFailed, err.Error(), "", ""); finishErr != nil {
			log.Printf("message triggers: %v", finishErr)
		}
		return
	}
	if err := d.store.FinishMessageDelivery(delivery.ID, model.DeliverySent, "", result.RoomID, result.EventID); err != nil {
		log.Printf("message triggers: sent but not recorded: %v", err)
		return
	}
	log.Printf("message triggers: trigger %s %q sent event %s (%s on card %s) to %s", trigger.ID, trigger.Name, event.ID, event.Kind, event.CardID, trigger.RecipientUserID)
}

// recordUnsent writes a delivery row for a trigger that matched but did not
// reach multichat, and logs why.
func (d *Dispatcher) recordUnsent(trigger *model.MessageTrigger, event model.CardEvent, boardID, message string, status model.MessageDeliveryStatus, reason string) {
	log.Printf("message triggers: trigger %s %q on event %s (%s on card %s): %s: %s", trigger.ID, trigger.Name, event.ID, event.Kind, event.CardID, status, reason)
	d.claim(&model.MessageDelivery{
		TriggerID: trigger.ID, BoardID: boardID, EventID: event.ID, CardID: event.CardID,
		RecipientUserID: trigger.RecipientUserID, RenderedMessage: message, Status: status, Error: reason,
	})
}

// claim inserts the delivery row. false means do not send: either this
// trigger already acted on this event, or the row could not be written — and
// a send with no row is a message nobody can find out about.
func (d *Dispatcher) claim(delivery *model.MessageDelivery) bool {
	err := d.store.ClaimMessageDelivery(delivery)
	if errors.Is(err, db.ErrAlreadyDelivered) {
		log.Printf("message triggers: trigger %s already has a delivery for event %s; not sending again", delivery.TriggerID, delivery.EventID)
		return false
	}
	if err != nil {
		log.Printf("message triggers: trigger %s event %s: %v", delivery.TriggerID, delivery.EventID, err)
		return false
	}
	return true
}
