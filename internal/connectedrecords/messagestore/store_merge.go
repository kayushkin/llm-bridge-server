package messagestore

import (
	"database/sql"
	"errors"
	"fmt"
)

// liveConversationID is the id of the conversation filed under key: its own
// when it is stored, the one it merged into when it was merged, and "" when
// the store has neither.
func liveConversationID(tx *sql.Tx, key ConversationKey) (id string, merged bool, err error) {
	err = tx.QueryRow(`SELECT id FROM conversations WHERE source = ? AND source_ref = ?`, key.Source, key.SourceRef).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRow(`SELECT merged_into FROM conversation_merges WHERE source = ? AND source_ref = ?`, key.Source, key.SourceRef).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// applyMerge merges one conversation into another.
func (w *batchWriter) applyMerge(merge ImportConversationMerge) error {
	if merge.Merged.Source == "" || merge.Merged.SourceRef == "" || merge.Into.Source == "" || merge.Into.SourceRef == "" {
		return fmt.Errorf("%w: merged and into each need source and source_ref", ErrInvalid)
	}
	if !contains(ConversationMergeRules, merge.Rule) {
		return fmt.Errorf("%w: rule %q is not one of %v", ErrInvalid, merge.Rule, ConversationMergeRules)
	}
	mergedID, _, err := liveConversationID(w.tx, merge.Merged)
	if err != nil {
		return err
	}
	intoID, _, err := liveConversationID(w.tx, merge.Into)
	if err != nil {
		return err
	}
	if mergedID == "" || intoID == "" {
		w.counts.ConversationMergesSkipped++
		return nil
	}
	if mergedID == intoID {
		return nil
	}
	var clash string
	err = w.tx.QueryRow(`SELECT day FROM conversation_days WHERE conversation_id = ? AND day IN (SELECT day FROM conversation_days WHERE conversation_id = ?) LIMIT 1`,
		mergedID, intoID).Scan(&clash)
	if err == nil {
		return fmt.Errorf("%w: %s and %s both have a conversation day %s; merging two days is not supported", ErrInvalid, mergedID, intoID, clash)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// The merged row as it was is read before anything moves.
	var source, sourceRef, rowJSON string
	err = w.tx.QueryRow(`SELECT source, source_ref, json_object(
			'seq', seq, 'id', id, 'platform', platform, 'source', source, 'source_ref', source_ref, 'title', title,
			'is_group', is_group, 'parent_source_ref', parent_source_ref, 'created_at', created_at, 'updated_at', updated_at,
			'participants', (SELECT json_group_array(json_object('handle', handle, 'display_name', display_name,
				'person_id', person_id, 'is_self', is_self)) FROM participants WHERE conversation_id = conversations.id))
		FROM conversations WHERE id = ?`, mergedID).Scan(&source, &sourceRef, &rowJSON)
	if err != nil {
		return err
	}
	moved, err := w.tx.Exec(`UPDATE messages SET conversation_id = ?, updated_at = ? WHERE conversation_id = ?`, intoID, w.now, mergedID)
	if err != nil {
		return err
	}
	messagesMoved, err := moved.RowsAffected()
	if err != nil {
		return err
	}
	if _, err := w.tx.Exec(`UPDATE conversation_days SET conversation_id = ?, updated_at = ? WHERE conversation_id = ?`, intoID, w.now, mergedID); err != nil {
		return err
	}
	// A handle in both keeps what the surviving conversation knows, and
	// learns what it does not.
	if _, err := w.tx.Exec(`INSERT INTO participants (conversation_id, handle, display_name, person_id, is_self)
			SELECT ?, handle, display_name, person_id, is_self FROM participants WHERE conversation_id = ? AND true
		ON CONFLICT (conversation_id, handle) DO UPDATE SET
			display_name = CASE WHEN display_name = '' THEN excluded.display_name ELSE display_name END,
			person_id    = CASE WHEN person_id    = '' THEN excluded.person_id    ELSE person_id END,
			is_self      = MAX(is_self, excluded.is_self)`, intoID, mergedID); err != nil {
		return err
	}
	if _, err := w.tx.Exec(`DELETE FROM participants WHERE conversation_id = ?`, mergedID); err != nil {
		return err
	}
	if _, err := w.tx.Exec(`INSERT INTO conversation_merges (conversation_id, source, source_ref, merged_into, rule, import_id, merged_at, messages_moved, row_json)
		VALUES (?,?,?,?,?,?,?,?,?)`, mergedID, source, sourceRef, intoID, merge.Rule, w.importID, w.now, messagesMoved, rowJSON); err != nil {
		return err
	}
	// A conversation merged earlier into the one merged now follows it.
	if _, err := w.tx.Exec(`UPDATE conversation_merges SET merged_into = ? WHERE merged_into = ?`, intoID, mergedID); err != nil {
		return err
	}
	if _, err := w.tx.Exec(`DELETE FROM conversations WHERE id = ?`, mergedID); err != nil {
		return err
	}
	for key, id := range w.conversations {
		if id == mergedID {
			w.conversations[key] = intoID
		}
	}
	w.counts.ConversationsMerged++
	return nil
}

// ErrMerged wraps the id of the conversation a merged one now lives in.
type ErrMerged struct{ ID, MergedInto string }

func (e ErrMerged) Error() string {
	return fmt.Sprintf("conversation %s was merged into %s", e.ID, e.MergedInto)
}

const mergeColumns = `conversation_id, source, source_ref, merged_into, rule, import_id, merged_at, messages_moved, row_json`

// ListConversationMerges answers GET /conversation-merges, newest first;
// mergedInto narrows it to the conversations merged into one.
func (s *Store) ListConversationMerges(mergedInto string, limit int) ([]ConversationMerge, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	query := `SELECT ` + mergeColumns + ` FROM conversation_merges`
	var args []any
	if mergedInto != "" {
		query += ` WHERE merged_into = ?`
		args = append(args, mergedInto)
	}
	rows, err := s.db.Query(query+` ORDER BY merged_at DESC, conversation_id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConversationMerge{}
	for rows.Next() {
		var merge ConversationMerge
		var rowJSON string
		if err := rows.Scan(&merge.ConversationID, &merge.Source, &merge.SourceRef, &merge.MergedInto, &merge.Rule, &merge.ImportID,
			&merge.MergedAt, &merge.MessagesMoved, &rowJSON); err != nil {
			return nil, err
		}
		merge.Row = []byte(rowJSON)
		out = append(out, merge)
	}
	return out, rows.Err()
}

// mergedInto is the conversation a merged conversation id now lives in, ""
// when the id was never merged.
func (s *Store) mergedInto(id string) (string, error) {
	var into string
	err := s.db.QueryRow(`SELECT merged_into FROM conversation_merges WHERE conversation_id = ?`, id).Scan(&into)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return into, err
}

// liveConversationIDs replaces each merged conversation id by the one it
// merged into, so a filter naming an old id still finds its messages.
func (s *Store) liveConversationIDs(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		into, err := s.mergedInto(id)
		if err != nil {
			return nil, err
		}
		if into != "" {
			id = into
		}
		out = append(out, id)
	}
	return out, nil
}
