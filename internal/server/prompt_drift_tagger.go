package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/llm-bridge/msg"
)

// When someone edits a rendered prompt file and adds a section, agent-store
// holds that drift for approval. Before a person looks at it, one single-shot
// model call labels the new sections: tags from the collection's existing
// vocabulary where they fit, and a one-line note on what the edit did. That is all the model does. The section text in the drift was cut
// out of the file by agent-store's splitter and the model's reply has no field
// that could change it; agent-store rejects a label aimed at anything but an
// added section.

const (
	promptDriftTaggerMaxTokens       = 1024
	promptDriftTaggerBodyPreviewSize = 1500
	promptDriftTaggerTimeout         = 90 * time.Second
)

const promptDriftTaggerSystemPrompt = `You label sections of an operator's system prompt for AI coding agents.
You are given the tags already in use and the sections that were just added to a prompt file.
For each added section, reply with its operation_index and 1 to 4 tags.
Reuse an existing tag whenever one fits; invent a new one only when none does. Tags are lowercase, one or two words, hyphenated.
Also reply with note: one plain sentence saying what the edit added.
You cannot change the sections' text and are not asked to judge it.`

func promptDriftTaggerOutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"note", "inserted_sections"},
		"properties": map[string]any{
			"note": map[string]any{"type": "string"},
			"inserted_sections": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"operation_index", "tags"},
					"properties": map[string]any{
						"operation_index": map[string]any{"type": "integer"},
						"tags":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}
}

// promptDriftTaggerOneShot runs the tagger's call on its configured harness
// instance. A missing or disabled instance is an error: the drift then stays
// held with no labels, which the Files page shows, and a person can still
// approve it with their own.
func (s *Server) promptDriftTaggerOneShot(ctx context.Context, req msg.OneShotRequest) ([]byte, error) {
	id := s.cfg.PromptDriftTaggerInstance
	if id == "" {
		return nil, fmt.Errorf("no prompt-drift-tagger instance configured (LLMBRIDGE_PROMPT_DRIFT_TAGGER_INSTANCE)")
	}
	inst, err := s.harnessStore.GetInstance(id)
	if err != nil {
		return nil, fmt.Errorf("prompt-drift-tagger instance %q: %w", id, err)
	}
	if !inst.Enabled {
		return nil, fmt.Errorf("prompt-drift-tagger instance %q is disabled", id)
	}
	raw, status, err := s.runOneShot(ctx, inst, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("prompt-drift-tagger oneshot returned %d: %s", status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// onPromptDriftsDetected is agent-store's hook. It returns at once; labelling
// runs in the background so a scan never waits on a model.
func (s *Server) onPromptDriftsDetected(drifts []agentstore.PromptDrift) {
	for _, drift := range drifts {
		if !drift.NeedsAnnotation() {
			continue
		}
		go func(drift agentstore.PromptDrift) {
			ctx, cancel := context.WithTimeout(context.Background(), promptDriftTaggerTimeout)
			defer cancel()
			if err := s.annotatePromptDrift(ctx, drift); err != nil {
				log.Printf("[prompt-drift] drift %d (%s) stays held with no labels: %v", drift.ID, drift.Path, err)
			}
		}(drift)
	}
}

func (s *Server) annotatePromptDrift(ctx context.Context, drift agentstore.PromptDrift) error {
	view, err := s.agentStore.GetPromptCollectionView(drift.CollectionID)
	if err != nil {
		return fmt.Errorf("read collection %d: %w", drift.CollectionID, err)
	}
	request, err := buildPromptDriftTaggerRequest(drift, view.Sections, s.cfg.PromptDriftTaggerModel)
	if err != nil {
		return err
	}
	raw, err := s.promptDriftTaggerOneShot(ctx, request)
	if err != nil {
		return err
	}
	annotation, err := decodePromptDriftTaggerReply(raw)
	if err != nil {
		return err
	}
	annotation.AnnotatedBy = "prompt-drift-tagger/" + s.cfg.PromptDriftTaggerModel
	if _, err := s.agentStore.SetPromptDriftAnnotation(drift.ID, *annotation); err != nil {
		return fmt.Errorf("agent-store refused the labels: %w", err)
	}
	log.Printf("[prompt-drift] drift %d (%s) labelled: %s", drift.ID, drift.Path, annotation.Note)
	return nil
}

func buildPromptDriftTaggerRequest(drift agentstore.PromptDrift, sections []agentstore.PromptSection, model string) (msg.OneShotRequest, error) {
	tagSet := map[string]bool{}
	for _, section := range sections {
		for _, tag := range section.Tags {
			tagSet[tag] = true
		}
	}
	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Prompt file edited: %s\n\nTags already in use: %s\n\nSections added:\n", drift.Path, strings.Join(tags, ", "))
	added := 0
	for index, operation := range drift.Operations {
		if operation.Kind != agentstore.PromptMarkdownEditInsert {
			continue
		}
		added++
		body := []rune(operation.Body)
		if len(body) > promptDriftTaggerBodyPreviewSize {
			body = append(body[:promptDriftTaggerBodyPreviewSize], []rune(" […]")...)
		}
		fmt.Fprintf(&prompt, "\n--- operation_index %d ---\n%s\n\n%s\n", index, operation.Heading, string(body))
	}
	if added == 0 {
		return msg.OneShotRequest{}, fmt.Errorf("drift %d adds no section; there is nothing to label", drift.ID)
	}
	schema, err := json.Marshal(promptDriftTaggerOutputSchema())
	if err != nil {
		return msg.OneShotRequest{}, err
	}
	return msg.OneShotRequest{
		Prompt:       prompt.String(),
		SystemPrompt: promptDriftTaggerSystemPrompt,
		Model:        model,
		Schema:       schema,
		MaxTokens:    promptDriftTaggerMaxTokens,
	}, nil
}

func decodePromptDriftTaggerReply(raw []byte) (*agentstore.PromptDriftAnnotation, error) {
	var reply msg.OneShotResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode oneshot reply: %w", err)
	}
	if len(reply.Parsed) == 0 {
		return nil, fmt.Errorf("oneshot returned no schema-conformant output (stop_reason=%q)", reply.StopReason)
	}
	var annotation agentstore.PromptDriftAnnotation
	if err := json.Unmarshal(reply.Parsed, &annotation); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return &annotation, nil
}
