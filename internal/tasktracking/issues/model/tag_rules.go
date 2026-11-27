package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A board's tag rules override its defaults for the cards that carry certain
// tags. A rule names one or more tags and any of the four defaults a board
// carries; it matches a card that carries ALL of its tags. Rules are ordered,
// and for each default the first matching rule that sets it wins — a field it
// leaves blank falls through to the next matching rule, and past the last to
// the board's own default. The tags are the card's noteboard tags, exactly as
// the card carries them.

// DefaultField names one of the four defaults a board or a tag rule may set.
// Its value is the JSON key both carry, so a resolution is keyed the way the
// board is written.
type DefaultField string

const (
	DefaultFieldPrincipal DefaultField = "default_principal_id"
	DefaultFieldAgent     DefaultField = "default_agent_id"
	DefaultFieldInstance  DefaultField = "default_instance_id"
	DefaultFieldBundle    DefaultField = "default_bundle_id"
)

// DefaultFields is every default, in the order a resolution reports them.
var DefaultFields = []DefaultField{DefaultFieldPrincipal, DefaultFieldAgent, DefaultFieldInstance, DefaultFieldBundle}

// DefaultValue is the board's own value for one default, "" when unset.
func (b *Board) DefaultValue(field DefaultField) string {
	switch field {
	case DefaultFieldPrincipal:
		return b.DefaultPrincipalID
	case DefaultFieldAgent:
		return b.DefaultAgentID
	case DefaultFieldInstance:
		return b.DefaultInstanceID
	case DefaultFieldBundle:
		return b.DefaultBundleID
	}
	return ""
}

// BoardTagRule is one stored rule. Position is its place in the board's
// order, 0 first; a lower position wins.
type BoardTagRule struct {
	ID                 string    `json:"id"`
	BoardID            string    `json:"board_id"`
	Position           int       `json:"position"`
	Tags               []string  `json:"tags"`
	DefaultPrincipalID string    `json:"default_principal_id,omitempty"`
	DefaultAgentID     string    `json:"default_agent_id,omitempty"`
	DefaultInstanceID  string    `json:"default_instance_id,omitempty"`
	DefaultBundleID    string    `json:"default_bundle_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// DefaultValue is the rule's value for one default, "" when it leaves it to
// the next matching rule or the board.
func (r *BoardTagRule) DefaultValue(field DefaultField) string {
	return defaultValueOf(field, r.DefaultPrincipalID, r.DefaultAgentID, r.DefaultInstanceID, r.DefaultBundleID)
}

// MatchesTags reports whether a card carrying cardTags carries every tag the
// rule names.
func (r *BoardTagRule) MatchesTags(cardTags map[string]bool) bool {
	for _, tag := range r.Tags {
		if !cardTags[tag] {
			return false
		}
	}
	return true
}

// BoardTagRules is a board's whole ordered list.
type BoardTagRules struct {
	BoardID string         `json:"board_id"`
	Rules   []BoardTagRule `json:"rules"`
}

// BoardTagRuleInput is one rule as PUT sends it. ID, when given, keeps an
// existing rule's id and created_at; omitted, the store mints one.
type BoardTagRuleInput struct {
	ID                 string   `json:"id,omitempty"`
	Tags               []string `json:"tags"`
	DefaultPrincipalID string   `json:"default_principal_id,omitempty"`
	DefaultAgentID     string   `json:"default_agent_id,omitempty"`
	DefaultInstanceID  string   `json:"default_instance_id,omitempty"`
	DefaultBundleID    string   `json:"default_bundle_id,omitempty"`
}

// DefaultValue is the input's value for one default.
func (r *BoardTagRuleInput) DefaultValue(field DefaultField) string {
	return defaultValueOf(field, r.DefaultPrincipalID, r.DefaultAgentID, r.DefaultInstanceID, r.DefaultBundleID)
}

func defaultValueOf(field DefaultField, principal, agent, instance, bundle string) string {
	switch field {
	case DefaultFieldPrincipal:
		return principal
	case DefaultFieldAgent:
		return agent
	case DefaultFieldInstance:
		return instance
	case DefaultFieldBundle:
		return bundle
	}
	return ""
}

// SetBoardTagRulesRequest replaces a board's rules wholesale, in the order
// given. Rules only mean anything relative to each other, so, like the
// priority ladder, there is no endpoint for editing one rule.
type SetBoardTagRulesRequest struct {
	Rules []BoardTagRuleInput `json:"rules"`
}

// Validate refuses a list that would store a rule nothing can match, a rule
// that changes nothing, or two rules nobody could tell apart. Tags and ids are
// never trimmed: a tag with a stray space would match no card and look fine.
func (r *SetBoardTagRulesRequest) Validate() error {
	if r.Rules == nil {
		return fmt.Errorf("rules is required: send the whole ordered list, or [] to remove every rule")
	}
	seenIDs := map[string]int{}
	seenTagSets := map[string]int{}
	for index, rule := range r.Rules {
		name := fmt.Sprintf("rules[%d]", index)
		if len(rule.Tags) == 0 {
			return fmt.Errorf("%s has no tags: a rule matches a card carrying all of its tags, so it needs at least one", name)
		}
		seenInRule := map[string]bool{}
		for _, tag := range rule.Tags {
			if tag == "" || tag != strings.TrimSpace(tag) {
				return fmt.Errorf("%s tag %q is empty or has surrounding whitespace, and nothing is trimmed: send the tag exactly as cards carry it", name, tag)
			}
			if seenInRule[tag] {
				return fmt.Errorf("%s names tag %q twice", name, tag)
			}
			seenInRule[tag] = true
		}
		described := fmt.Sprintf("%s (tags %s)", name, strings.Join(rule.Tags, " + "))
		key := tagSetKey(rule.Tags)
		if earlier, ok := seenTagSets[key]; ok {
			return fmt.Errorf("%s matches exactly the same tags as rules[%d]; merge them into one rule", described, earlier)
		}
		seenTagSets[key] = index
		setsAny := false
		for _, field := range DefaultFields {
			value := rule.DefaultValue(field)
			if value == "" {
				continue
			}
			setsAny = true
			if value != strings.TrimSpace(value) {
				return fmt.Errorf("%s %s %q has surrounding whitespace, and nothing is trimmed: send the owner's id exactly", described, field, value)
			}
		}
		if !setsAny {
			return fmt.Errorf("%s sets no default; give it at least one of default_principal_id, default_agent_id, default_instance_id, default_bundle_id", described)
		}
		if rule.ID != "" {
			if earlier, ok := seenIDs[rule.ID]; ok {
				return fmt.Errorf("%s and rules[%d] both carry id %s", name, earlier, rule.ID)
			}
			seenIDs[rule.ID] = index
		}
	}
	return nil
}

func tagSetKey(tags []string) string {
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x00")
}

// Where a resolved default came from.
const (
	DefaultSourceBoard   = "board"
	DefaultSourceTagRule = "tag_rule"
)

// DefaultSource says which setting a resolved default was read from. For a tag
// rule it carries the rule's id, and its tags and position for display.
type DefaultSource struct {
	Kind         string   `json:"kind"`
	RuleID       string   `json:"rule_id,omitempty"`
	RuleTags     []string `json:"rule_tags,omitempty"`
	RulePosition *int     `json:"rule_position,omitempty"`
}

// EffectiveDefault is one resolved default and where it came from.
type EffectiveDefault struct {
	Value  string        `json:"value"`
	Source DefaultSource `json:"source"`
}

// EffectiveDefaults is what a card with these tags gets on this board. A field
// absent from Defaults has no value anywhere: no matching rule sets it and the
// board does not either.
type EffectiveDefaults struct {
	BoardID        string                            `json:"board_id"`
	CardID         string                            `json:"card_id,omitempty"`
	Tags           []string                          `json:"tags"`
	MatchedRuleIDs []string                          `json:"matched_rule_ids"`
	Defaults       map[DefaultField]EffectiveDefault `json:"defaults"`
}

// ResolveEffectiveDefaults is the one place the precedence lives: for each
// default, the first matching rule (by position) that sets it, else the
// board's own value, else nothing.
func ResolveEffectiveDefaults(board *Board, rules []BoardTagRule, cardTags []string) EffectiveDefaults {
	carried := make(map[string]bool, len(cardTags))
	for _, tag := range cardTags {
		carried[tag] = true
	}
	ordered := append([]BoardTagRule(nil), rules...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })

	tags := cardTags
	if tags == nil {
		tags = []string{}
	}
	resolved := EffectiveDefaults{BoardID: board.ID, Tags: tags, MatchedRuleIDs: []string{}, Defaults: map[DefaultField]EffectiveDefault{}}
	var matched []BoardTagRule
	for _, rule := range ordered {
		if rule.MatchesTags(carried) {
			matched = append(matched, rule)
			resolved.MatchedRuleIDs = append(resolved.MatchedRuleIDs, rule.ID)
		}
	}
	for _, field := range DefaultFields {
		for _, rule := range matched {
			if value := rule.DefaultValue(field); value != "" {
				position := rule.Position
				resolved.Defaults[field] = EffectiveDefault{Value: value, Source: DefaultSource{
					Kind: DefaultSourceTagRule, RuleID: rule.ID, RuleTags: rule.Tags, RulePosition: &position,
				}}
				break
			}
		}
		if _, set := resolved.Defaults[field]; set {
			continue
		}
		if value := board.DefaultValue(field); value != "" {
			resolved.Defaults[field] = EffectiveDefault{Value: value, Source: DefaultSource{Kind: DefaultSourceBoard}}
		}
	}
	return resolved
}
