package model

import "testing"

func TestTagRulePrecedence(t *testing.T) {
	board := &Board{ID: "b1", DefaultInstanceID: "inst-board", DefaultAgentID: "12"}
	rules := []BoardTagRule{
		// Deliberately out of order: position decides, not slice order.
		{ID: "broad", Position: 1, Tags: []string{"cat:product"}, DefaultInstanceID: "inst-product", DefaultBundleID: "6"},
		{ID: "narrow", Position: 0, Tags: []string{"cat:product", "urgency:high"}, DefaultInstanceID: "inst-urgent"},
	}

	both := ResolveEffectiveDefaults(board, rules, []string{"urgency:high", "cat:product", "email"})
	if got := both.Defaults[DefaultFieldInstance]; got.Value != "inst-urgent" || got.Source.RuleID != "narrow" {
		t.Errorf("instance = %+v, want inst-urgent from the rule at position 0", got)
	}
	if got := both.Defaults[DefaultFieldBundle]; got.Value != "6" || got.Source.RuleID != "broad" {
		t.Errorf("bundle = %+v, want 6 falling through to the next matching rule", got)
	}
	if got := both.Defaults[DefaultFieldAgent]; got.Value != "12" || got.Source.Kind != DefaultSourceBoard {
		t.Errorf("agent = %+v, want 12 from the board, which no rule sets", got)
	}
	if _, set := both.Defaults[DefaultFieldPrincipal]; set {
		t.Error("principal resolved to something though nothing sets it")
	}
	if len(both.MatchedRuleIDs) != 2 || both.MatchedRuleIDs[0] != "narrow" {
		t.Errorf("matched = %v, want [narrow broad] in position order", both.MatchedRuleIDs)
	}

	// A rule matches only when the card carries ALL of its tags.
	onlyUrgent := ResolveEffectiveDefaults(board, rules, []string{"urgency:high"})
	if got := onlyUrgent.Defaults[DefaultFieldInstance]; got.Value != "inst-board" || got.Source.Kind != DefaultSourceBoard {
		t.Errorf("instance for a card missing cat:product = %+v, want the board's", got)
	}
	if len(onlyUrgent.MatchedRuleIDs) != 0 {
		t.Errorf("matched = %v, want none", onlyUrgent.MatchedRuleIDs)
	}

	// Tags match exactly: case and prefixes are not folded.
	if got := ResolveEffectiveDefaults(board, rules, []string{"Cat:Product"}).Defaults[DefaultFieldInstance]; got.Source.Kind != DefaultSourceBoard {
		t.Errorf("a differently cased tag matched a rule: %+v", got)
	}
}

func TestTagRulesValidation(t *testing.T) {
	for name, request := range map[string]SetBoardTagRulesRequest{
		"missing list":       {},
		"no tags":            {Rules: []BoardTagRuleInput{{DefaultInstanceID: "x"}}},
		"no defaults":        {Rules: []BoardTagRuleInput{{Tags: []string{"a"}}}},
		"padded tag":         {Rules: []BoardTagRuleInput{{Tags: []string{"a "}, DefaultInstanceID: "x"}}},
		"tag twice":          {Rules: []BoardTagRuleInput{{Tags: []string{"a", "a"}, DefaultInstanceID: "x"}}},
		"same tag set twice": {Rules: []BoardTagRuleInput{{Tags: []string{"a", "b"}, DefaultInstanceID: "x"}, {Tags: []string{"b", "a"}, DefaultBundleID: "6"}}},
		"same id twice":      {Rules: []BoardTagRuleInput{{ID: "r1", Tags: []string{"a"}, DefaultInstanceID: "x"}, {ID: "r1", Tags: []string{"b"}, DefaultInstanceID: "x"}}},
		"padded default id":  {Rules: []BoardTagRuleInput{{Tags: []string{"a"}, DefaultInstanceID: " x"}}},
	} {
		if err := request.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	empty := SetBoardTagRulesRequest{Rules: []BoardTagRuleInput{}}
	if err := empty.Validate(); err != nil {
		t.Errorf("an empty list (remove every rule) was refused: %v", err)
	}
}
