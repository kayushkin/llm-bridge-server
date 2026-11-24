package toolstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// TagUnreviewed marks a harness tool that a harness reported and nobody has
// reviewed yet. RecordObservedHarnessTools creates such rows switched off; a
// person reviews one by replacing its tags (PATCH /tools/{id}) and, if it
// should be on, enabling it (POST /tools/{id}/enable).
const TagUnreviewed = "unreviewed"

// ObservedHarnessToolsRequest is the body of POST /harness-tools/observed: the
// built-in tool names one harness reported at the start of a session. Harness
// is llm-bridge-server's harness id, passed through as sent.
type ObservedHarnessToolsRequest struct {
	Harness   string   `json:"harness"`
	ToolNames []string `json:"tool_names"`
}

// ObservedHarnessToolsResponse answers POST /harness-tools/observed. Created
// lists the reported names that had no row and now have one, switched off and
// tagged unreviewed; Seen counts the distinct names whose last_seen_at was set.
type ObservedHarnessToolsResponse struct {
	Created []string `json:"created"`
	Seen    int      `json:"seen"`
}

// ErrInvalidObservedHarnessTools wraps every refusal of a report's content.
var ErrInvalidObservedHarnessTools = errors.New("invalid observed harness tools")

// ErrObservedNameHeldByAnotherKind is returned when <harness>.<name> is
// already the name of a tool that is not kind=harness.
var ErrObservedNameHeldByAnotherKind = errors.New("name is held by a tool of another kind")

func validateObservedHarnessTools(request ObservedHarnessToolsRequest) error {
	if request.Harness == "" {
		return fmt.Errorf("%w: harness is required", ErrInvalidObservedHarnessTools)
	}
	if len(request.ToolNames) == 0 {
		return fmt.Errorf("%w: tool_names is empty", ErrInvalidObservedHarnessTools)
	}
	for _, name := range request.ToolNames {
		switch {
		case name == "":
			return fmt.Errorf("%w: a tool name is empty", ErrInvalidObservedHarnessTools)
		case strings.Contains(name, "."):
			return fmt.Errorf("%w: tool name %q contains a dot", ErrInvalidObservedHarnessTools, name)
		case strings.IndexFunc(name, unicode.IsSpace) >= 0:
			return fmt.Errorf("%w: tool name %q contains whitespace", ErrInvalidObservedHarnessTools, name)
		case strings.HasPrefix(name, "mcp__"):
			return fmt.Errorf("%w: tool name %q is an MCP tool, not a built-in tool of the harness", ErrInvalidObservedHarnessTools, name)
		}
	}
	return nil
}

// unreviewedHarnessToolDescription says who reported a tool, when, and that
// nobody has looked at it.
func unreviewedHarnessToolDescription(harness string, reportedAt int64) string {
	return fmt.Sprintf("Reported by the %s harness on %s and created switched off; nobody has reviewed it yet.",
		harness, time.Unix(reportedAt, 0).UTC().Format(time.RFC3339))
}

// RecordObservedHarnessTools records that harness offered each of toolNames
// in a session. A name with a row gets last_seen_at set to now and nothing
// else changed. A name without one gets a kind=harness row, enabled=false and
// tagged unreviewed, so a tool nobody registered is switched off everywhere
// until a person reviews it. One write transaction covers the whole report,
// and each insert is ON CONFLICT DO NOTHING, so two sessions reporting the
// same new name at once create one row and both succeed.
func (s *Store) RecordObservedHarnessTools(ctx context.Context, request ObservedHarnessToolsRequest) (ObservedHarnessToolsResponse, error) {
	if err := validateObservedHarnessTools(request); err != nil {
		return ObservedHarnessToolsResponse{}, err
	}
	reportedAt := now()
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservedHarnessToolsResponse{}, err
	}
	defer transaction.Rollback()

	response := ObservedHarnessToolsResponse{Created: []string{}}
	alreadyRecorded := map[string]bool{}
	tags := encodeStrings([]string{TagUnreviewed})
	for _, harnessToolName := range request.ToolNames {
		if alreadyRecorded[harnessToolName] {
			continue
		}
		alreadyRecorded[harnessToolName] = true
		rowName := HarnessToolRowName(request.Harness, harnessToolName)

		inserted, err := transaction.ExecContext(ctx, `
			INSERT INTO tools (name, description, kind, tags, harness, harness_tool_name, last_seen_at, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
			ON CONFLICT (name) DO NOTHING
		`, rowName, unreviewedHarnessToolDescription(request.Harness, reportedAt), string(KindHarness), tags,
			request.Harness, harnessToolName, reportedAt, reportedAt, reportedAt)
		if err != nil {
			return ObservedHarnessToolsResponse{}, fmt.Errorf("create %s: %w", rowName, err)
		}
		insertedCount, err := inserted.RowsAffected()
		if err != nil {
			return ObservedHarnessToolsResponse{}, err
		}
		if insertedCount == 1 {
			response.Created = append(response.Created, harnessToolName)
			response.Seen++
			continue
		}

		updated, err := transaction.ExecContext(ctx,
			`UPDATE tools SET last_seen_at = ? WHERE name = ? AND kind = ?`,
			reportedAt, rowName, string(KindHarness))
		if err != nil {
			return ObservedHarnessToolsResponse{}, fmt.Errorf("mark %s seen: %w", rowName, err)
		}
		updatedCount, err := updated.RowsAffected()
		if err != nil {
			return ObservedHarnessToolsResponse{}, err
		}
		if updatedCount != 1 {
			return ObservedHarnessToolsResponse{}, fmt.Errorf("%w: %s", ErrObservedNameHeldByAnotherKind, rowName)
		}
		response.Seen++
	}
	if err := transaction.Commit(); err != nil {
		return ObservedHarnessToolsResponse{}, err
	}
	return response, nil
}

// ToolPatch is the body of PATCH /tools/{id}: the fields a person changes when
// reviewing a tool. A nil field is left as it is. Tags replaces the whole
// list.
type ToolPatch struct {
	Tags        *[]string `json:"tags,omitempty"`
	Description *string   `json:"description,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

// ErrInvalidToolPatch wraps every refusal of a patch's content: one that
// changes nothing, or an empty tag.
var ErrInvalidToolPatch = errors.New("invalid tool patch")

// PatchTool changes only the fields patch sets, and returns the row as stored.
func (s *Store) PatchTool(id int64, patch ToolPatch) (*Tool, error) {
	var assignments []string
	var args []any
	if patch.Tags != nil {
		for _, tag := range *patch.Tags {
			if strings.TrimSpace(tag) == "" {
				return nil, fmt.Errorf("%w: a tag is empty", ErrInvalidToolPatch)
			}
		}
		assignments = append(assignments, "tags = ?")
		args = append(args, encodeStrings(*patch.Tags))
	}
	if patch.Description != nil {
		assignments = append(assignments, "description = ?")
		args = append(args, *patch.Description)
	}
	if patch.Enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, boolToInt(*patch.Enabled))
	}
	if len(assignments) == 0 {
		return nil, fmt.Errorf("%w: it names no field to change; send any of tags, description, enabled", ErrInvalidToolPatch)
	}
	assignments = append(assignments, "updated_at = ?")
	args = append(args, now(), id)
	result, err := s.db.Exec(`UPDATE tools SET `+strings.Join(assignments, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if count == 0 {
		return nil, ErrNotFound
	}
	return s.GetTool(id)
}
