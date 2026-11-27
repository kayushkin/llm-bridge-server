package tools

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
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
// same new name at once create one row and both succeed: the second insert
// waits for the first transaction and then does nothing.
func (s *Store) RecordObservedHarnessTools(ctx context.Context, request ObservedHarnessToolsRequest) (ObservedHarnessToolsResponse, error) {
	if err := validateObservedHarnessTools(request); err != nil {
		return ObservedHarnessToolsResponse{}, err
	}
	reportedAt := now()
	response := ObservedHarnessToolsResponse{Created: []string{}}
	err := postgresstorage.InTransaction(ctx, s.db, func(transaction *sql.Tx) error {
		// Reports take turns, as SQLite's write lock made them: two reports
		// naming the same new tools in different orders would otherwise wait
		// on each other's rows, and Postgres would fail one as a deadlock.
		if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('tools.record_observed_harness_tools'))`); err != nil {
			return fmt.Errorf("waiting for another report of harness tools: %w", err)
		}
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
				VALUES ($1, $2, $3, $4, $5, $6, $7, FALSE, $8, $9)
				ON CONFLICT (name) DO NOTHING
			`, rowName, unreviewedHarnessToolDescription(request.Harness, reportedAt), string(KindHarness), tags,
				request.Harness, harnessToolName, reportedAt, reportedAt, reportedAt)
			if err != nil {
				return fmt.Errorf("create %s: %w", rowName, err)
			}
			insertedCount, err := inserted.RowsAffected()
			if err != nil {
				return err
			}
			if insertedCount == 1 {
				response.Created = append(response.Created, harnessToolName)
				response.Seen++
				continue
			}

			updated, err := transaction.ExecContext(ctx,
				`UPDATE tools SET last_seen_at = $1 WHERE name = $2 AND kind = $3`,
				reportedAt, rowName, string(KindHarness))
			if err != nil {
				return fmt.Errorf("mark %s seen: %w", rowName, err)
			}
			updatedCount, err := updated.RowsAffected()
			if err != nil {
				return err
			}
			if updatedCount != 1 {
				return fmt.Errorf("%w: %s", ErrObservedNameHeldByAnotherKind, rowName)
			}
			response.Seen++
		}
		return nil
	})
	if err != nil {
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
func (s *Store) PatchTool(ctx context.Context, id int64, patch ToolPatch) (*Tool, error) {
	var assignments []string
	var args []any
	if patch.Tags != nil {
		for _, tag := range *patch.Tags {
			if strings.TrimSpace(tag) == "" {
				return nil, fmt.Errorf("%w: a tag is empty", ErrInvalidToolPatch)
			}
		}
		args = append(args, encodeStrings(*patch.Tags))
		assignments = append(assignments, fmt.Sprintf("tags = $%d", len(args)))
	}
	if patch.Description != nil {
		args = append(args, *patch.Description)
		assignments = append(assignments, fmt.Sprintf("description = $%d", len(args)))
	}
	if patch.Enabled != nil {
		args = append(args, *patch.Enabled)
		assignments = append(assignments, fmt.Sprintf("enabled = $%d", len(args)))
	}
	if len(assignments) == 0 {
		return nil, fmt.Errorf("%w: it names no field to change; send any of tags, description, enabled", ErrInvalidToolPatch)
	}
	args = append(args, now())
	assignments = append(assignments, fmt.Sprintf("updated_at = $%d", len(args)))
	args = append(args, id)
	result, err := s.db.ExecContext(ctx, `UPDATE tools SET `+strings.Join(assignments, ", ")+fmt.Sprintf(` WHERE id = $%d`, len(args)), args...)
	if err != nil {
		return nil, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if count == 0 {
		return nil, ErrNotFound
	}
	return s.GetTool(ctx, id)
}
