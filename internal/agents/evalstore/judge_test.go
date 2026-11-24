package evalstore

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestTheBlindShuffleIsAPermutationFixedByTheCaseAlone(t *testing.T) {
	candidates := []string{"model-original", "model-small", "model-large", "model-four", "model-five"}
	first := BlindLabelMapping("eval_case_000007", candidates)
	if !maps(first, BlindLabelMapping("eval_case_000007", candidates)) {
		t.Fatal("the same case shuffled two ways")
	}
	labels := make([]string, 0, len(first))
	models := make([]string, 0, len(first))
	for label, model := range first {
		labels = append(labels, label)
		models = append(models, model)
	}
	slices.Sort(labels)
	slices.Sort(models)
	if !slices.Equal(labels, []string{"A", "B", "C", "D", "E"}) || !slices.Equal(models, slices.Sorted(slices.Values(candidates))) {
		t.Fatalf("mapping %v is not a permutation of the candidates under A..E", first)
	}
	// Across many cases the original model must not sit under one label:
	// a judge that learned "A is the original" would be judging blind no more.
	labelOfOriginal := map[string]int{}
	for i := range 200 {
		for label, model := range BlindLabelMapping(fmt.Sprintf("eval_case_%06d", i), candidates) {
			if model == "model-original" {
				labelOfOriginal[label]++
			}
		}
	}
	if len(labelOfOriginal) != 5 {
		t.Errorf("over 200 cases the original sat under only %v", labelOfOriginal)
	}
	for label, count := range labelOfOriginal {
		if count < 15 {
			t.Errorf("label %s held the original %d times in 200; the shuffle is lopsided: %v", label, count, labelOfOriginal)
		}
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestTheJudgePromptShowsEveryLabelAndNoAnswerForAMissingOne(t *testing.T) {
	suite := &Suite{JudgeInstructions: "INSTRUCTIONS"}
	c := &Case{SystemPrompt: "SYSTEM", UserPrompt: "USER"}
	mapping := map[string]string{"A": "model-small", "B": "model-original"}
	prompt, err := judgeUserPrompt(suite, c, mapping, map[string]*Answer{"model-original": {Parsed: json.RawMessage(`{"k":"v"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"INSTRUCTIONS", "##### TASK SYSTEM PROMPT #####\nSYSTEM", "##### TASK USER PROMPT #####\nUSER",
		"=== Candidate A ===\nNO ANSWER", "=== Candidate B ===\n{\n \"k\": \"v\"\n}"} {
		if !strings.Contains(prompt, part) {
			t.Errorf("prompt lacks %q:\n%s", part, prompt)
		}
	}
	if strings.Contains(prompt, "model-") {
		t.Error("the prompt names a model")
	}
	if system := judgeSystemPrompt(8); !strings.Contains(system, "up to eight candidate answers, labeled A to H") {
		t.Errorf("system prompt: %s", system)
	}
}

func TestAVerdictMustRankAndJudgeEveryLabelOnce(t *testing.T) {
	mapping := map[string]string{"A": "model-small", "B": "model-original", "C": "model-large"}
	valid := json.RawMessage(`{"ranking":["B","C","A"],"verdicts":[
		{"label":"A","acceptable":false,"failure_kind":"wrong_card","reason":"r"},
		{"label":"B","acceptable":true,"failure_kind":"none","reason":"r"},
		{"label":"C","acceptable":true,"failure_kind":"other","reason":"r"}]}`)
	lines, err := interpretVerdict(valid, mapping)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]VerdictCandidate{}
	for _, line := range lines {
		byModel[line.ModelID] = line
	}
	if byModel["model-original"].Rank != 1 || byModel["model-large"].Rank != 2 || byModel["model-small"].Rank != 3 || byModel["model-small"].Acceptable {
		t.Errorf("unshuffled lines %+v", byModel)
	}
	for name, malformed := range map[string]string{
		"ranking misses a label":  `{"ranking":["B","C"],"verdicts":[{"label":"A"},{"label":"B"},{"label":"C"}]}`,
		"ranking repeats a label": `{"ranking":["B","B","A"],"verdicts":[{"label":"A"},{"label":"B"},{"label":"C"}]}`,
		"verdict repeats a label": `{"ranking":["B","C","A"],"verdicts":[{"label":"A"},{"label":"A"},{"label":"C"}]}`,
		"not a verdict":           `{"ranking":"B"}`,
	} {
		if _, err := interpretVerdict(json.RawMessage(malformed), mapping); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheJudgeSchemaOffersOnlyTheLabelsAndTheSuitesFailureKinds(t *testing.T) {
	schema := mustJSON(judgeSchema(3, []string{"wrong_card", "no_answer", "none"}))
	for _, part := range []string{`"enum":["A","B","C"]`, `"enum":["wrong_card","no_answer","none"]`, `"required":["ranking","verdicts"]`} {
		if !strings.Contains(schema, part) {
			t.Errorf("schema lacks %s: %s", part, schema)
		}
	}
}
