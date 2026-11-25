package evalstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
)

// BlindLabelMapping shuffles the candidates under the labels A, B, C, … with
// a shuffle seeded by the case id alone, so every judge of a case sees the
// same order, a resumed run rebuilds the same prompt (and so the same
// idempotency key holds), and no judge can learn a model from its position.
func BlindLabelMapping(caseID string, candidates []string) map[string]string {
	digest := sha256.Sum256([]byte(caseID + "-shuffle"))
	random := rand.New(rand.NewPCG(binary.BigEndian.Uint64(digest[0:8]), binary.BigEndian.Uint64(digest[8:16])))
	order := append([]string(nil), candidates...)
	random.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	mapping := make(map[string]string, len(order))
	for i, label := range labelsFor(len(order)) {
		mapping[label] = order[i]
	}
	return mapping
}

var numberWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}

func numberWord(n int) string {
	if n < len(numberWords) {
		return numberWords[n]
	}
	return strconv.Itoa(n)
}

// judgeSystemPrompt is what every judge is told, for n candidates. It is the
// Python bench's text with the count filled in.
func judgeSystemPrompt(n int) string {
	labels := labelsFor(n)
	count := numberWord(n)
	return fmt.Sprintf("You are a strict, careful evaluator. You will see a task given to a model (its system prompt and user prompt) "+
		"and up to %s candidate answers, labeled %s to %s, in random order. The candidates come from different models; you do not know "+
		"which is which. Rank all %s labels best to worst, and for each label say whether it is acceptable, the main failure kind "+
		"(%s when acceptable and flawless), and one line of reason. A candidate that says NO ANSWER failed to produce output: rank it "+
		"last and mark it unacceptable with failure_kind %s.", count, labels[0], labels[n-1], count, FailureKindNone, FailureKindNoAnswer)
}

// judgeSchema forces a ranking of every label and one verdict per label.
func judgeSchema(n int, failureKinds []string) map[string]any {
	labels := labelsFor(n)
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"ranking", "verdicts"},
		"properties": map[string]any{
			"ranking": map[string]any{"type": "array", "description": "All labels, best first.", "items": map[string]any{"type": "string", "enum": labels}},
			"verdicts": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"label", "acceptable", "failure_kind", "reason"},
				"properties": map[string]any{
					"label":        map[string]any{"type": "string", "enum": labels},
					"acceptable":   map[string]any{"type": "boolean"},
					"failure_kind": map[string]any{"type": "string", "enum": failureKinds},
					"reason":       map[string]any{"type": "string"},
				},
			}},
		},
	}
}

// noAnswerText stands in for a candidate that produced nothing.
const noAnswerText = "NO ANSWER"

// judgeUserPrompt shows the judge the suite's instructions, the task and
// every candidate under its label, in label order.
func judgeUserPrompt(suite *Suite, c *Case, mapping map[string]string, answers map[string]*Answer) (string, error) {
	labels := labelsFor(len(mapping))
	parts := make([]string, len(labels))
	for i, label := range labels {
		text := noAnswerText
		if a := answers[mapping[label]]; a != nil && a.HasAnswer() {
			var indented bytes.Buffer
			if err := json.Indent(&indented, a.Parsed, "", " "); err != nil {
				return "", fmt.Errorf("answer %s is not JSON: %w", a.ID, err)
			}
			text = indented.String()
		}
		parts[i] = fmt.Sprintf("=== Candidate %s ===\n%s", label, text)
	}
	return fmt.Sprintf("%s\n\n##### TASK SYSTEM PROMPT #####\n%s\n\n##### TASK USER PROMPT #####\n%s\n\n##### CANDIDATE ANSWERS #####\n%s",
		suite.JudgeInstructions, c.SystemPrompt, c.UserPrompt, strings.Join(parts, "\n\n")), nil
}
