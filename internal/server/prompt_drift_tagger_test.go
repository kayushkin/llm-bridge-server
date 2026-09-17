package server

import (
	"encoding/json"
	"strings"
	"testing"

	agentstore "github.com/kayushkin/agent-store"
)

func TestPromptDriftTaggerRequestNamesOnlyAddedSectionsAndExistingTags(t *testing.T) {
	drift := agentstore.PromptDrift{ID: 7, Path: "/home/x/CLAUDE.md", Operations: []agentstore.PromptDriftOperation{
		{Kind: agentstore.PromptMarkdownEditUpdate, SectionID: 1, Heading: "## Old", Body: "SECRET-EDITED-BODY"},
		{Kind: agentstore.PromptMarkdownEditInsert, Heading: "## Reminders", Body: strings.Repeat("x", 5000)},
	}}
	sections := []agentstore.PromptSection{{Tags: []string{"scheduler", "stores"}}, {Tags: []string{"stores"}}}
	request, err := buildPromptDriftTaggerRequest(drift, sections, "some-model")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Prompt, "operation_index 1") || strings.Contains(request.Prompt, "operation_index 0") {
		t.Fatalf("prompt should name the insert only:\n%s", request.Prompt)
	}
	if strings.Contains(request.Prompt, "SECRET-EDITED-BODY") {
		t.Fatal("an edited section's body was sent; only added sections are labelled")
	}
	if !strings.Contains(request.Prompt, "Tags already in use: scheduler, stores") {
		t.Fatalf("existing tags missing:\n%s", request.Prompt)
	}
	if len(request.Prompt) > 3000 {
		t.Fatalf("a 5000-char body was not cut down: prompt is %d chars", len(request.Prompt))
	}
	if request.Model != "some-model" || len(request.Schema) == 0 {
		t.Fatal("model or schema not set")
	}
}

func TestPromptDriftTaggerRequestRefusesADriftThatAddsNothing(t *testing.T) {
	drift := agentstore.PromptDrift{Operations: []agentstore.PromptDriftOperation{{Kind: agentstore.PromptMarkdownEditDelete, SectionID: 3}}}
	if _, err := buildPromptDriftTaggerRequest(drift, nil, "m"); err == nil {
		t.Fatal("built a request with nothing to label")
	}
}

func TestPromptDriftTaggerReplyWithoutParsedOutputIsAnError(t *testing.T) {
	if _, err := decodePromptDriftTaggerReply([]byte(`{"text":"sure!","stop_reason":"end_turn"}`)); err == nil {
		t.Fatal("free text was accepted as labels")
	}
	parsed, _ := json.Marshal(map[string]any{"parsed": map[string]any{"note": "added reminders", "inserted_sections": []map[string]any{{"operation_index": 1, "title": "Reminders", "tags": []string{"scheduler"}}}}})
	annotation, err := decodePromptDriftTaggerReply(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if annotation.Note != "added reminders" || annotation.InsertedSections[0].OperationIndex != 1 {
		t.Fatalf("annotation = %+v", annotation)
	}
}
