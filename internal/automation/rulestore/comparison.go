package rulestore

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Comparing a shadow rule with what actually happened.
//
// A shadow rule exists to answer one question before it is allowed to act:
// would it have done what the binary it replaces did, no more and no less?
// The comparison answers both halves from the owners' current state:
//
//   - agreed / not yet: for each effect a shadow run planned, does the owner now
//     show its result — the card in that column, the tag on the session, the
//     session in that folder? reached_at minus decided_at says how much sooner
//     (negative) or later the rule decided than the result appeared.
//   - missed: for a move, which cards arrived in the target column since the
//     rule was activated, linked to a session, with no run of this rule about
//     that session? Those are moves something else made that the rule did not
//     decide.
//
// A message cannot be checked this way and is reported as not checkable.

// EffectCheck is one planned effect and whether its result is visible now.
type EffectCheck struct {
	RunID       string `json:"run_id"`
	SubjectID   string `json:"subject_id"`
	DecidedAt   int64  `json:"decided_at"`
	Kind        string `json:"kind"`
	Path        string `json:"path"`
	Checkable   bool   `json:"checkable"`
	Reached     bool   `json:"reached"`
	ReachedAt   string `json:"reached_at,omitempty"`
	LeadSeconds *int64 `json:"lead_seconds,omitempty"` // reached_at - decided_at
	Detail      string `json:"detail,omitempty"`
}

// MissedArrival is a card that reached a rule's target column with no run of
// the rule about any session linked to it.
type MissedArrival struct {
	CardID     string   `json:"card_id"`
	ToColumnID string   `json:"to_column_id"`
	ArrivedAt  string   `json:"arrived_at"`
	SessionIDs []string `json:"session_ids"`
}

// Comparison is what GET /rules/{id}/comparison answers.
type Comparison struct {
	RuleID        string          `json:"rule_id"`
	Since         int64           `json:"since"`
	RunsChecked   int             `json:"runs_checked"`
	RunsTruncated bool            `json:"runs_truncated"`
	Reached       int             `json:"reached"`
	NotReached    int             `json:"not_reached"`
	NotCheckable  int             `json:"not_checkable"`
	Missed        int             `json:"missed"`
	Effects       []EffectCheck   `json:"effects"`
	MissedCards   []MissedArrival `json:"missed_cards"`
}

// comparisonRunLimit bounds the owner reads one comparison makes.
const comparisonRunLimit = 500

// CompareRule checks a rule's shadow runs since a moment against the owners.
// since 0 means the rule's first activation.
func (s *Store) CompareRule(ctx context.Context, ruleID string, since int64, kanban *KanbanStore, bridge *LLMBridge) (*Comparison, error) {
	rule, err := s.GetRule(ruleID)
	if err != nil {
		return nil, err
	}
	if since == 0 {
		for _, a := range rule.Activations {
			if a.Action == "activate" {
				since = a.ActedAt
				break
			}
		}
		if since == 0 {
			return nil, fmt.Errorf("%w: rule %s has never been activated, so there is nothing to compare", ErrConflict, ruleID)
		}
	}
	runs, err := s.ListRuns(RunFilter{RuleID: ruleID, Since: since, Limit: comparisonRunLimit + 1})
	if err != nil {
		return nil, err
	}
	report := &Comparison{RuleID: ruleID, Since: since, Effects: []EffectCheck{}, MissedCards: []MissedArrival{}}
	if len(runs) > comparisonRunLimit {
		runs, report.RunsTruncated = runs[:comparisonRunLimit], true
	}
	report.RunsChecked = len(runs)

	decidedSessions := map[string]bool{}
	for _, run := range runs {
		decidedSessions[run.SubjectID] = true
		for _, effect := range run.PlannedEffects {
			check, err := s.checkEffect(ctx, run, effect, kanban, bridge)
			if err != nil {
				return nil, err
			}
			switch {
			case !check.Checkable:
				report.NotCheckable++
			case check.Reached:
				report.Reached++
			default:
				report.NotReached++
			}
			report.Effects = append(report.Effects, check)
		}
	}
	// Runs older than the truncation still count as decided, so a missed card
	// is only one no run at all covers.
	if report.RunsTruncated {
		all, err := s.ListRuns(RunFilter{RuleID: ruleID, Since: since, Limit: 1 << 30})
		if err != nil {
			return nil, err
		}
		for _, run := range all {
			decidedSessions[run.SubjectID] = true
		}
	}

	for _, revision := range rule.Revisions {
		if revision.ID != rule.ActiveRevisionID {
			continue
		}
		for _, effect := range revision.Definition.Effects {
			if effect.Kind != EffectMoveLinkedCards {
				continue
			}
			missed, err := missedArrivals(ctx, kanban, effect, since, decidedSessions)
			if err != nil {
				return nil, err
			}
			report.MissedCards = append(report.MissedCards, missed...)
		}
	}
	report.Missed = len(report.MissedCards)
	return report, nil
}

func (s *Store) checkEffect(ctx context.Context, run Run, effect PlannedEffect, kanban *KanbanStore, bridge *LLMBridge) (EffectCheck, error) {
	check := EffectCheck{RunID: run.ID, SubjectID: run.SubjectID, DecidedAt: run.DecidedAt, Kind: effect.Kind, Path: effect.Path, Checkable: true}
	body, _ := effect.Body.(map[string]any)
	switch effect.Kind {
	case EffectMoveLinkedCards:
		cardID := cardIDFromMovePath(effect.Path)
		card, err := kanban.Card(ctx, cardID)
		if err != nil {
			return check, fmt.Errorf("read card %s: %w", cardID, err)
		}
		for _, p := range card.Placements {
			if p.BoardID == body["board_id"] && p.ColumnID == body["column_id"] {
				check.Reached, check.ReachedAt = true, p.UpdatedAt
			}
		}
	case EffectAddSessionTag:
		tags, err := kanban.SessionTags(ctx, run.SubjectID)
		if err != nil {
			return check, err
		}
		check.Reached = slices.Contains(tags, fmt.Sprint(body["tag"]))
	case EffectMoveSessionToFolder:
		// Asked of the bridge, not rule-store's copy: a bulk move
		// (POST /admin/archive-old) sends no feed frame, so the copy can
		// still show the old folder after the session was archived.
		session, found, err := bridge.GetSession(ctx, run.SubjectID)
		if err != nil {
			return check, err
		}
		if !found {
			check.Checkable, check.Detail = false, "the bridge no longer has the session"
			return check, nil
		}
		check.Reached = session.FolderName == fmt.Sprint(body["folder"])
	default:
		check.Checkable, check.Detail = false, "no owner state shows whether this effect happened"
	}
	if check.Reached && check.ReachedAt != "" {
		if reachedAt, err := time.Parse(time.RFC3339Nano, check.ReachedAt); err == nil {
			lead := reachedAt.Unix() - run.DecidedAt
			check.LeadSeconds = &lead
		}
	}
	return check, nil
}

// cardIDFromMovePath reads the card id out of /api/cards/{id}/move.
func cardIDFromMovePath(path string) string {
	const prefix, suffix = "/api/cards/", "/move"
	if len(path) > len(prefix)+len(suffix) {
		return path[len(prefix) : len(path)-len(suffix)]
	}
	return ""
}

func missedArrivals(ctx context.Context, kanban *KanbanStore, effect Effect, since int64, decidedSessions map[string]bool) ([]MissedArrival, error) {
	arrivals, err := kanban.ColumnCards(ctx, effect.BoardID, effect.ToColumnID)
	if err != nil {
		return nil, err
	}
	missed := []MissedArrival{}
	for _, arrival := range arrivals {
		arrivedAt, err := time.Parse(time.RFC3339Nano, arrival.ArrivedAt)
		if err != nil {
			return nil, fmt.Errorf("card %s has an unreadable placement time %q: %w", arrival.CardID, arrival.ArrivedAt, err)
		}
		if arrivedAt.Unix() < since {
			continue
		}
		sessions := arrival.SessionIDs
		covered := false
		for _, sessionID := range sessions {
			covered = covered || decidedSessions[sessionID]
		}
		if len(sessions) > 0 && !covered {
			missed = append(missed, MissedArrival{CardID: arrival.CardID, ToColumnID: effect.ToColumnID, ArrivedAt: arrival.ArrivedAt, SessionIDs: sessions})
		}
	}
	return missed, nil
}
