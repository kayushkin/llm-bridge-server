package rulestore

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The rule language, and what keeps it small.
//
// A definition is a flat list of conditions, all of which must hold, and an
// ordered list of effects. There is no "or", no nesting and no expression: a
// rule that needs "or" is two rules. That is the limit that stops a rules
// store becoming a programming language nobody can review, and it is why the
// condition and effect kinds are a served vocabulary rather than open text.
//
// A condition reads only facts about the subject that changed — its own
// fields, its age, its entity tags, the cards linked to it. It never reads
// some other record. An effect is one call to the service that owns what it
// changes; rule-store never writes anywhere itself.

// SourceSession is a session in llm-bridge-server, followed on its
// /session-events feed.
const SourceSession = "llm-bridge-server.session"

// Sources is every source a rule can watch, served at GET /vocabulary.
var Sources = []string{SourceSession}

// Condition kinds.
const (
	ConditionFieldIn            = "field_in"
	ConditionFieldNotIn         = "field_not_in"
	ConditionUpdatedBefore      = "updated_before"
	ConditionSessionTagPresent  = "session_tag_present"
	ConditionSessionTagAbsent   = "session_tag_absent"
	ConditionLinkedCardInColumn = "linked_card_in_column"
)

// ConditionKinds is every condition kind, in the order evaluation runs them:
// the ones read from the session copy first, the ones that call kanban-store
// last, so a rule that cannot match costs no call.
var ConditionKinds = []string{
	ConditionFieldIn, ConditionFieldNotIn, ConditionUpdatedBefore,
	ConditionSessionTagPresent, ConditionSessionTagAbsent, ConditionLinkedCardInColumn,
}

// SessionFields is every session field field_in and field_not_in may test.
// Each is a string llm-bridge-server sets on GET /sessions.
// status_reason is why the state last changed (llm-bridge msg.SessionStateReason,
// served by GET /session-taxonomy as state_reasons): idle_reaped tells a
// session whose finished process the reaper stopped from one stopped mid-turn.
var SessionFields = []string{"state", "purpose", "folder_name", "harness", "origin", "type", "status_reason"}

// Effect kinds.
const (
	EffectMoveLinkedCards     = "move_linked_cards"
	EffectAddSessionTag       = "add_session_tag"
	EffectSendSessionMessage  = "send_session_message"
	EffectMoveSessionToFolder = "move_session_to_folder"
)

// EffectKinds is every effect kind, served at GET /vocabulary.
var EffectKinds = []string{EffectMoveLinkedCards, EffectAddSessionTag, EffectSendSessionMessage, EffectMoveSessionToFolder}

// Modes. A shadow rule records the calls its effects would make and makes
// none. A live rule makes them. Switch a rule to live only after its
// comparison agrees with the job it replaces, and turn that job off in the
// same sitting, so two writers never act on one card or session.
const (
	ModeShadow = "shadow"
	ModeLive   = "live"
)

// Modes is every mode a rule can be activated in.
var Modes = []string{ModeShadow, ModeLive}

// Condition is one test. Kind decides which other fields mean anything.
type Condition struct {
	Kind     string   `json:"kind"`
	Field    string   `json:"field,omitempty"`     // field_in, field_not_in
	Values   []string `json:"values,omitempty"`    // field_in, field_not_in
	Age      string   `json:"age,omitempty"`       // updated_before: a Go duration, "30m", "48h"
	Tag      string   `json:"tag,omitempty"`       // session_tag_present, session_tag_absent
	BoardID  string   `json:"board_id,omitempty"`  // linked_card_in_column
	ColumnID string   `json:"column_id,omitempty"` // linked_card_in_column
}

// Effect is one call to an owner. Kind decides which other fields mean anything.
type Effect struct {
	Kind       string `json:"kind"`
	BoardID    string `json:"board_id,omitempty"`     // move_linked_cards
	ToColumnID string `json:"to_column_id,omitempty"` // move_linked_cards
	Tag        string `json:"tag,omitempty"`          // add_session_tag
	Text       string `json:"text,omitempty"`         // send_session_message
	Folder     string `json:"folder,omitempty"`       // move_session_to_folder
}

// Definition is a rule revision's content.
type Definition struct {
	Conditions []Condition `json:"conditions"`
	Effects    []Effect    `json:"effects"`
}

// Vocabulary is what GET /vocabulary serves, so no caller hardcodes a list.
type Vocabulary struct {
	Sources        []string `json:"sources"`
	ConditionKinds []string `json:"condition_kinds"`
	SessionFields  []string `json:"session_fields"`
	EffectKinds    []string `json:"effect_kinds"`
	Modes          []string `json:"modes"`
	RuleStates     []string `json:"rule_states"`
}

// CurrentVocabulary is the vocabulary this binary understands.
func CurrentVocabulary() Vocabulary {
	return Vocabulary{
		Sources: Sources, ConditionKinds: ConditionKinds, SessionFields: SessionFields,
		EffectKinds: EffectKinds, Modes: Modes, RuleStates: RuleStates,
	}
}

// uuidPattern is the shape of a kanban-store board or column id.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func oneOf(value string, allowed []string, what string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return invalidf("unknown %s %q: use one of %s", what, value, strings.Join(allowed, ", "))
}

// ParseDefinition decodes and validates a definition for a source. Unknown
// JSON keys are refused, so a misspelled field is a 400, not a condition that
// silently tests nothing.
func ParseDefinition(source string, raw json.RawMessage) (Definition, error) {
	var d Definition
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return d, invalidf("definition: %v", err)
	}
	return d, d.Validate(source)
}

// Validate checks a definition against the vocabulary and against itself.
func (d Definition) Validate(source string) error {
	if err := oneOf(source, Sources, "source"); err != nil {
		return err
	}
	if len(d.Conditions) == 0 {
		return invalidf("a rule needs at least one condition; a rule that matches every change to every session is a mistake")
	}
	if len(d.Effects) == 0 {
		return invalidf("a rule needs at least one effect")
	}
	cardColumnsByBoard := map[string]bool{}
	for i, c := range d.Conditions {
		where := fmt.Sprintf("conditions[%d] (%s)", i, c.Kind)
		if err := oneOf(c.Kind, ConditionKinds, "condition kind"); err != nil {
			return fmt.Errorf("conditions[%d]: %w", i, err)
		}
		if err := c.validateFields(where); err != nil {
			return err
		}
		if c.Kind == ConditionLinkedCardInColumn {
			cardColumnsByBoard[c.BoardID] = true
		}
	}
	for i, e := range d.Effects {
		where := fmt.Sprintf("effects[%d] (%s)", i, e.Kind)
		if err := oneOf(e.Kind, EffectKinds, "effect kind"); err != nil {
			return fmt.Errorf("effects[%d]: %w", i, err)
		}
		if err := e.validateFields(where); err != nil {
			return err
		}
		// A move acts on the cards a condition found. Without that condition
		// it would move every card linked to the session, from any column —
		// including one a person has just closed by hand.
		if e.Kind == EffectMoveLinkedCards && !cardColumnsByBoard[e.BoardID] {
			return invalidf("%s: needs a linked_card_in_column condition on board %s, which chooses the cards it moves", where, e.BoardID)
		}
	}
	return nil
}

// validateFields checks that a condition carries what its kind needs and
// nothing it ignores.
func (c Condition) validateFields(where string) error {
	set := map[string]bool{
		"field": c.Field != "", "values": len(c.Values) > 0, "age": c.Age != "",
		"tag": c.Tag != "", "board_id": c.BoardID != "", "column_id": c.ColumnID != "",
	}
	var needs []string
	switch c.Kind {
	case ConditionFieldIn, ConditionFieldNotIn:
		needs = []string{"field", "values"}
		if err := oneOf(c.Field, SessionFields, "session field"); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	case ConditionUpdatedBefore:
		needs = []string{"age"}
		age, err := time.ParseDuration(c.Age)
		if err != nil || age <= 0 {
			return invalidf("%s: age %q must be a positive Go duration such as 30m or 48h", where, c.Age)
		}
	case ConditionSessionTagPresent, ConditionSessionTagAbsent:
		needs = []string{"tag"}
	case ConditionLinkedCardInColumn:
		needs = []string{"board_id", "column_id"}
		if !uuidPattern.MatchString(c.BoardID) || !uuidPattern.MatchString(c.ColumnID) {
			return invalidf("%s: board_id and column_id must be kanban-store ids; a column is named by id, never by name", where)
		}
	}
	return exactlyTheseFields(where, set, needs)
}

func (e Effect) validateFields(where string) error {
	set := map[string]bool{
		"board_id": e.BoardID != "", "to_column_id": e.ToColumnID != "",
		"tag": e.Tag != "", "text": strings.TrimSpace(e.Text) != "", "folder": e.Folder != "",
	}
	var needs []string
	switch e.Kind {
	case EffectMoveLinkedCards:
		needs = []string{"board_id", "to_column_id"}
		if !uuidPattern.MatchString(e.BoardID) || !uuidPattern.MatchString(e.ToColumnID) {
			return invalidf("%s: board_id and to_column_id must be kanban-store ids", where)
		}
	case EffectAddSessionTag:
		needs = []string{"tag"}
	case EffectSendSessionMessage:
		needs = []string{"text"}
	case EffectMoveSessionToFolder:
		needs = []string{"folder"}
	}
	return exactlyTheseFields(where, set, needs)
}

func exactlyTheseFields(where string, set map[string]bool, needs []string) error {
	for _, field := range needs {
		if !set[field] {
			return invalidf("%s: %s is required", where, field)
		}
	}
	for field, present := range set {
		if present && !slices.Contains(needs, field) {
			return invalidf("%s: %s means nothing for this kind; remove it", where, field)
		}
	}
	return nil
}
