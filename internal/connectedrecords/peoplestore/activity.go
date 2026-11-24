package peoplestore

import (
	"fmt"
	"strings"
)

// ReportHandleActivity replaces the activity rows of every handle in the
// report, in one transaction: all of them or none. Each handle is normalized
// as an identity is; a handle named twice in one report is a 400, since one
// of the two would silently win.
func (s *Store) ReportHandleActivity(report HandleActivityReport) (*HandleActivityResult, error) {
	source := strings.TrimSpace(report.Source)
	if !contains(SuggestionSources, source) {
		return nil, fmt.Errorf("%w: unknown source %q: use one of %s", ErrInvalid, report.Source, strings.Join(SuggestionSources, ", "))
	}
	if report.Activities == nil {
		return nil, fmt.Errorf("%w: activities is required", ErrInvalid)
	}
	normalized := make([]HandleActivity, 0, len(report.Activities))
	seen := map[string]bool{}
	for index, activity := range report.Activities {
		handle, err := NormalizeIdentity(IdentityInput{Kind: activity.Kind, Value: activity.Value})
		if err != nil {
			return nil, fmt.Errorf("activities[%d]: %w", index, err)
		}
		if activity.LastActivityAt < 0 || activity.MessageCount < 0 {
			return nil, fmt.Errorf("%w: activities[%d]: last_activity_at and message_count must not be negative", ErrInvalid, index)
		}
		key := handle.Kind + "\x00" + handle.Value
		if seen[key] {
			return nil, fmt.Errorf("%w: activities[%d]: %s %s is named twice", ErrInvalid, index, handle.Kind, handle.Value)
		}
		seen[key] = true
		activity.Kind, activity.Value = handle.Kind, handle.Value
		normalized = append(normalized, activity)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO handle_activity (kind, value, source, last_activity_at, message_count, direct_chat, observed_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (kind, value) DO UPDATE SET source = excluded.source, last_activity_at = excluded.last_activity_at,
			message_count = excluded.message_count, direct_chat = excluded.direct_chat, observed_at = excluded.observed_at`)
	if err != nil {
		return nil, err
	}
	defer statement.Close()
	observedAt := now()
	for _, activity := range normalized {
		if _, err := statement.Exec(activity.Kind, activity.Value, source, activity.LastActivityAt, activity.MessageCount, activity.DirectChat, observedAt); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &HandleActivityResult{Updated: len(normalized)}, nil
}
