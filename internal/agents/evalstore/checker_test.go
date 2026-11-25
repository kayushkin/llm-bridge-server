package evalstore

import (
	"encoding/json"
	"strings"
	"testing"
)

func checkByName(results []CheckResult) map[string]CheckResult {
	byName := map[string]CheckResult{}
	for _, r := range results {
		byName[r.CheckName] = r
	}
	return byName
}

func TestEmailFilingCheckerPassesAFaithfulFiling(t *testing.T) {
	c := &Case{UserPrompt: syntheticFilingPrompt("e1", "e2")}
	answer := json.RawMessage(`{"filings":[` + syntheticFiling("e1", syntheticCardA) + `,` + syntheticFiling("e2", syntheticCardB) + `]}`)
	results := checkByName(emailFilingChecker{}.Check(c, answer))
	for _, name := range []string{CheckEveryEmailFiledOnce, CheckNoInventedCardID, CheckEveryFilingHasCard, CheckLinkLabelWithinLimit} {
		if !results[name].Passed {
			t.Errorf("%s failed: %+v", name, results[name])
		}
	}
	if results[CheckNewCards].Count != 0 || results[CheckCardUpdates].Count != 0 {
		t.Errorf("tallies %+v %+v", results[CheckNewCards], results[CheckCardUpdates])
	}
	features := emailFilingChecker{}.Features(c)
	if features["cards"] != 2 || features["emails"] != 2 {
		t.Errorf("features %v", features)
	}
}

func TestEmailFilingCheckerCatchesEachDefect(t *testing.T) {
	c := &Case{UserPrompt: syntheticFilingPrompt("e1", "e2")}
	invented := "cccccccc-0000-0000-0000-000000000003"
	longLabel := strings.Repeat("é", 51) // 51 characters, 102 bytes
	answer := json.RawMessage(`{"filings":[
		{"email_ref":"e1","card_ids":["` + invented + `"],"link_label":"` + longLabel + `"},
		{"email_ref":"e1","card_ids":[],"link_label":"x"},
		{"email_ref":"e9","new_card":{"title":"A new card"},"link_label":"x"}],
		"card_updates":[{"card_id":"` + invented + `"},{"card_id":"` + syntheticCardA + `"}]}`)
	results := checkByName(emailFilingChecker{}.Check(c, answer))
	if r := results[CheckEveryEmailFiledOnce]; r.Passed || !strings.Contains(r.Detail, "e2") || !strings.Contains(r.Detail, "e9") {
		t.Errorf("coverage: %+v", r)
	}
	if r := results[CheckNoInventedCardID]; r.Passed || r.Count != 2 {
		t.Errorf("invented card ids: %+v, want 2 (one filed, one updated)", r)
	}
	if r := results[CheckEveryFilingHasCard]; r.Passed || r.Count != 1 {
		t.Errorf("filing without card: %+v", r)
	}
	if r := results[CheckLinkLabelWithinLimit]; r.Passed || r.Count != 1 {
		t.Errorf("label length counts characters, not bytes: %+v", r)
	}
	if results[CheckNewCards].Count != 1 || results[CheckCardUpdates].Count != 2 {
		t.Errorf("tallies %+v %+v", results[CheckNewCards], results[CheckCardUpdates])
	}

	exactly50 := json.RawMessage(`{"filings":[{"email_ref":"e1","card_ids":["` + syntheticCardA + `"],"link_label":"` + strings.Repeat("é", 50) + `"},` +
		syntheticFiling("e2", syntheticCardA) + `]}`)
	if r := checkByName(emailFilingChecker{}.Check(c, exactly50))[CheckLinkLabelWithinLimit]; !r.Passed {
		t.Errorf("a 50-character label failed: %+v", r)
	}
}

func TestEmailFilingChoicesCompareCardsAndNewCardTitles(t *testing.T) {
	checker := emailFilingChecker{}
	c := &Case{UserPrompt: syntheticFilingPrompt("e1")}
	a := checker.Choices(c, json.RawMessage(`{"filings":[{"email_ref":"e1","card_ids":["`+syntheticCardB+`","`+syntheticCardA+`"]}]}`))
	b := checker.Choices(c, json.RawMessage(`{"filings":[{"email_ref":"e1","card_ids":["`+syntheticCardA+`","`+syntheticCardB+`"]}]}`))
	if a["e1"] != b["e1"] {
		t.Errorf("card order changed the choice: %q vs %q", a["e1"], b["e1"])
	}
	withNewCard := checker.Choices(c, json.RawMessage(`{"filings":[{"email_ref":"e1","card_ids":["`+syntheticCardA+`","`+syntheticCardB+`"],"new_card":{"title":"T"}}]}`))
	if withNewCard["e1"] == a["e1"] {
		t.Error("a new card did not change the choice")
	}
}

const syntheticDemoPrompt = `PROJECT: example-api

Write 2 message(s).

--- key: msg-1
kind: question
from: Ada Example <ada@example.test> — lead; asks questions
what they want: Ask a question.

--- key: msg-2
kind: reconsider
from: Grace Sample <grace@example.test> — manager; changes scope
what they want: Question a decision.`

func demoAnswer(messages ...[2]string) json.RawMessage {
	var parts []string
	for _, m := range messages {
		parts = append(parts, `{"key":`+mustJSON(m[0])+`,"subject":"s","body":`+mustJSON(m[1])+`}`)
	}
	return json.RawMessage(`{"messages":[` + strings.Join(parts, ",") + `]}`)
}

func TestDemoMailCheckerReadsTheBriefAndChecksKeys(t *testing.T) {
	c := &Case{UserPrompt: syntheticDemoPrompt}
	if f := (demoMailChecker{}).Features(c); f["messages"] != 2 {
		t.Errorf("features %v", f)
	}
	good := checkByName(demoMailChecker{}.Check(c, demoAnswer([2]string{"msg-1", "Hi Grace,\n\nA question.\n\nAda"}, [2]string{"msg-2", "Hello team,\n\nGrace"})))
	if !good[CheckMessageKeysMatchBrief].Passed || !good[CheckSenderDoesNotGreetSelf].Passed {
		t.Errorf("a good answer failed: %+v", good)
	}
	wrongKeys := checkByName(demoMailChecker{}.Check(c, demoAnswer([2]string{"msg-1", "Hi,"}, [2]string{"msg-1", "Hi,"})))
	if wrongKeys[CheckMessageKeysMatchBrief].Passed {
		t.Error("a repeated key with one missing passed")
	}
}

func TestDemoMailCheckerCatchesASenderGreetingThemselves(t *testing.T) {
	c := &Case{UserPrompt: syntheticDemoPrompt}
	for _, body := range []string{"Hi Ada,\n\nQuick question.", "Ada,\n\nQuick question.", "\n\nHey ada — quick one.", "Good morning Ada!"} {
		results := checkByName(demoMailChecker{}.Check(c, demoAnswer([2]string{"msg-1", body}, [2]string{"msg-2", "Hi Ada,"})))
		if r := results[CheckSenderDoesNotGreetSelf]; r.Passed || r.Count != 1 {
			t.Errorf("%q from Ada: %+v, want one self-greeting", body, r)
		}
	}
	for _, body := range []string{"Hi Grace,\n\nThanks.\n\nAda", "Adam,\n\nNot the sender.", "Quick question about paging.\n\n— Ada"} {
		results := checkByName(demoMailChecker{}.Check(c, demoAnswer([2]string{"msg-1", body}, [2]string{"msg-2", "Hi Ada,"})))
		if r := results[CheckSenderDoesNotGreetSelf]; !r.Passed {
			t.Errorf("%q from Ada was flagged: %+v", body, r)
		}
	}
}

func TestAMissingAnswerFailsOnlyTheAnsweredCheck(t *testing.T) {
	results := runChecker(emailFilingChecker{}, &Case{UserPrompt: syntheticFilingPrompt("e1")}, &Answer{Error: "operation failed"})
	if len(results) != 1 || results[0].CheckName != CheckAnswered || results[0].Passed {
		t.Errorf("results %+v", results)
	}
}
