package rulestore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// SessionFacts is everything a condition may read beyond the session copy:
// the cards linked to the session and the session's entity tags, both owned
// by kanban-store. The evaluator gets this and nothing else, so a rule cannot
// grow into a query over other records.
type SessionFacts interface {
	LinkedCardPlacements(ctx context.Context, sessionID string) ([]CardPlacement, error)
	SessionTags(ctx context.Context, sessionID string) ([]string, error)
}

// CardPlacement is one card linked to a session, on one board, in one column.
type CardPlacement struct {
	CardID   string `json:"card_id"`
	BoardID  string `json:"board_id"`
	ColumnID string `json:"column_id"`
}

// PlannedEffect is one call an effect makes to its owner, exactly as it would
// be sent. In shadow mode it is only recorded.
type PlannedEffect struct {
	Kind    string `json:"kind"`
	Service string `json:"service"` // llm-bridge-server | kanban-store
	Method  string `json:"method"`
	Path    string `json:"path"`
	Body    any    `json:"body,omitempty"`
	// Result is what the owner answered, on a live run only.
	Result *EffectResult `json:"result,omitempty"`
}

// EffectResult is the owner's answer to one effect's call, kept as it came.
type EffectResult struct {
	Status   int    `json:"status"`
	Body     string `json:"body"`
	Error    string `json:"error,omitempty"` // the call never got an answer
	CalledAt int64  `json:"called_at"`
}

// Decision is what evaluating one revision against one subject found.
type Decision struct {
	Matched        bool
	Facts          map[string]any
	PlannedEffects []PlannedEffect
}

// EvaluateSession decides one definition against one session. Conditions run
// in the vocabulary's order, so the ones read from the copy rule-store holds
// run before the ones that call kanban-store, and the first false one stops
// evaluation.
func EvaluateSession(ctx context.Context, definition Definition, subject Subject, at time.Time, facts SessionFacts) (Decision, error) {
	session, err := ParseSessionSnapshot(subject.Snapshot)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{Facts: map[string]any{"trigger_key": subject.TriggerKey()}}
	conditions := slices.Clone(definition.Conditions)
	slices.SortStableFunc(conditions, func(a, b Condition) int {
		return slices.Index(ConditionKinds, a.Kind) - slices.Index(ConditionKinds, b.Kind)
	})

	var tags []string
	tagsRead := false
	var placements []CardPlacement
	placementsRead := false
	// matchedCards is, per board, the cards a linked_card_in_column condition
	// found. A move acts on these and on no other card.
	matchedCards := map[string][]CardPlacement{}

	for _, c := range conditions {
		holds := false
		switch c.Kind {
		case ConditionFieldIn, ConditionFieldNotIn:
			value, err := session.Field(c.Field)
			if err != nil {
				return Decision{}, err
			}
			decision.Facts[c.Field] = value
			holds = slices.Contains(c.Values, value) == (c.Kind == ConditionFieldIn)
		case ConditionUpdatedBefore:
			updatedAt, err := time.Parse(time.RFC3339Nano, session.UpdatedAt)
			if err != nil {
				return Decision{}, fmt.Errorf("session %s has an unreadable updated_at %q: %w", session.SessionID, session.UpdatedAt, err)
			}
			age, err := time.ParseDuration(c.Age)
			if err != nil {
				return Decision{}, err
			}
			decision.Facts["updated_at"] = session.UpdatedAt
			holds = at.Sub(updatedAt) > age
		case ConditionSessionTagPresent, ConditionSessionTagAbsent:
			if !tagsRead {
				if tags, err = facts.SessionTags(ctx, session.SessionID); err != nil {
					return Decision{}, fmt.Errorf("read the tags of session %s: %w", session.SessionID, err)
				}
				tagsRead = true
				decision.Facts["session_tags"] = tags
			}
			holds = slices.Contains(tags, c.Tag) == (c.Kind == ConditionSessionTagPresent)
		case ConditionLinkedCardInColumn:
			if !placementsRead {
				if placements, err = facts.LinkedCardPlacements(ctx, session.SessionID); err != nil {
					return Decision{}, fmt.Errorf("read the cards linked to session %s: %w", session.SessionID, err)
				}
				placementsRead = true
				decision.Facts["linked_cards"] = placements
			}
			for _, p := range placements {
				if p.BoardID == c.BoardID && p.ColumnID == c.ColumnID {
					matchedCards[c.BoardID] = append(matchedCards[c.BoardID], p)
					holds = true
				}
			}
		default:
			return Decision{}, fmt.Errorf("condition kind %q has no evaluator", c.Kind)
		}
		if !holds {
			return decision, nil
		}
	}

	decision.Matched = true
	for _, e := range definition.Effects {
		switch e.Kind {
		case EffectMoveLinkedCards:
			for _, card := range matchedCards[e.BoardID] {
				decision.PlannedEffects = append(decision.PlannedEffects, PlannedEffect{
					Kind: e.Kind, Service: "kanban-store", Method: "POST", Path: "/api/cards/" + card.CardID + "/move",
					Body: map[string]any{"board_id": e.BoardID, "column_id": e.ToColumnID},
				})
			}
		case EffectAddSessionTag:
			decision.PlannedEffects = append(decision.PlannedEffects, PlannedEffect{
				Kind: e.Kind, Service: "kanban-store", Method: "POST", Path: "/api/entities/session/" + session.SessionID + "/tags",
				Body: map[string]any{"tag": e.Tag},
			})
		case EffectSendSessionMessage:
			// client_request_id is filled with the run id when the run is
			// recorded, so a live send repeated after a crash is one message.
			decision.PlannedEffects = append(decision.PlannedEffects, PlannedEffect{
				Kind: e.Kind, Service: "llm-bridge-server", Method: "POST", Path: "/sessions/" + session.SessionID + "/send",
				Body: map[string]any{"message": e.Text},
			})
		case EffectMoveSessionToFolder:
			decision.PlannedEffects = append(decision.PlannedEffects, PlannedEffect{
				Kind: e.Kind, Service: "llm-bridge-server", Method: "PUT", Path: "/sessions/" + session.SessionID + "/folder",
				Body: map[string]any{"folder": e.Folder},
			})
		default:
			return Decision{}, fmt.Errorf("effect kind %q has no planner", e.Kind)
		}
	}
	return decision, nil
}

// factsJSON renders a decision's facts for the run row.
func factsJSON(facts map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(facts)
	return json.RawMessage(raw), err
}
