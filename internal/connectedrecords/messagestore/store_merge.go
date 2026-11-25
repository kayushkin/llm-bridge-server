package messagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// liveConversationSeq is the number of the conversation owner files under
// key: its own when it is stored, the one it merged into when it was merged,
// and 0 when the owner's records hold neither.
func liveConversationSeq(ctx context.Context, db queryer, owner string, key ConversationKey) (seq int64, merged bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT seq FROM conversations WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		owner, key.Source, key.SourceRef).Scan(&seq)
	if err == nil {
		return seq, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	err = db.QueryRowContext(ctx, `SELECT merged_into_seq FROM conversation_merges WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`,
		owner, key.Source, key.SourceRef).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return seq, err == nil, err
}

// applyMerge merges one of the owner's conversations into another.
func (w *batchWriter) applyMerge(merge ImportConversationMerge) error {
	if merge.Merged.Source == "" || merge.Merged.SourceRef == "" || merge.Into.Source == "" || merge.Into.SourceRef == "" {
		return fmt.Errorf("%w: merged and into each need source and source_ref", ErrInvalid)
	}
	if !contains(ConversationMergeRules, merge.Rule) {
		return fmt.Errorf("%w: rule %q is not one of %v", ErrInvalid, merge.Rule, ConversationMergeRules)
	}
	mergedSeq, _, err := liveConversationSeq(w.ctx, w.tx, w.owner, merge.Merged)
	if err != nil {
		return err
	}
	intoSeq, _, err := liveConversationSeq(w.ctx, w.tx, w.owner, merge.Into)
	if err != nil {
		return err
	}
	if mergedSeq == 0 || intoSeq == 0 {
		w.counts.ConversationMergesSkipped++
		return nil
	}
	if mergedSeq == intoSeq {
		return nil
	}
	mergedID := postgresstorage.FormatRecordID(ConversationIDPrefix, mergedSeq)
	intoID := postgresstorage.FormatRecordID(ConversationIDPrefix, intoSeq)
	var clash string
	err = w.queryRow(`SELECT day FROM conversation_days WHERE conversation_seq = $1 AND day IN (SELECT day FROM conversation_days WHERE conversation_seq = $2) LIMIT 1`,
		mergedSeq, intoSeq).Scan(&clash)
	if err == nil {
		return fmt.Errorf("%w: %s and %s both have a conversation day %s; merging two days is not supported", ErrInvalid, mergedID, intoID, clash)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// The merged row as it was is read before anything moves.
	rowJSON, source, sourceRef, err := conversationRowJSON(w.ctx, w.tx, mergedSeq)
	if err != nil {
		return err
	}
	moved, err := w.exec(`UPDATE messages SET conversation_seq = $1, updated_at = $2 WHERE conversation_seq = $3`, intoSeq, w.now, mergedSeq)
	if err != nil {
		return err
	}
	messagesMoved, err := moved.RowsAffected()
	if err != nil {
		return err
	}
	if _, err := w.exec(`UPDATE conversation_days SET conversation_seq = $1, updated_at = $2 WHERE conversation_seq = $3`, intoSeq, w.now, mergedSeq); err != nil {
		return err
	}
	// A handle in both keeps what the surviving conversation knows, and
	// learns what it does not.
	if _, err := w.exec(`INSERT INTO participants (owner_principal_id, conversation_seq, handle, display_name, person_id, is_self)
			SELECT owner_principal_id, $1, handle, display_name, person_id, is_self FROM participants WHERE conversation_seq = $2
		ON CONFLICT (conversation_seq, handle) DO UPDATE SET
			display_name = CASE WHEN participants.display_name = '' THEN excluded.display_name ELSE participants.display_name END,
			person_id    = CASE WHEN participants.person_id    = '' THEN excluded.person_id    ELSE participants.person_id END,
			is_self      = participants.is_self OR excluded.is_self`, intoSeq, mergedSeq); err != nil {
		return err
	}
	if _, err := w.exec(`DELETE FROM participants WHERE conversation_seq = $1`, mergedSeq); err != nil {
		return err
	}
	if _, err := w.exec(`INSERT INTO conversation_merges (conversation_seq, owner_principal_id, source, source_ref, merged_into_seq, rule, import_seq, merged_at, messages_moved, row_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, mergedSeq, w.owner, source, sourceRef, intoSeq, merge.Rule, w.importSeq, w.now, messagesMoved, rowJSON); err != nil {
		return err
	}
	// A conversation merged earlier into the one merged now follows it.
	if _, err := w.exec(`UPDATE conversation_merges SET merged_into_seq = $1 WHERE merged_into_seq = $2`, intoSeq, mergedSeq); err != nil {
		return err
	}
	if _, err := w.exec(`DELETE FROM conversations WHERE seq = $1`, mergedSeq); err != nil {
		return err
	}
	for key, seq := range w.conversations {
		if seq == mergedSeq {
			w.conversations[key] = intoSeq
		}
	}
	w.counts.ConversationsMerged++
	return nil
}

// conversationRowJSON is a conversation's row and participants as one JSON
// object, with the keys, order and values the standalone store wrote: ids in
// their public form, flags as 0 or 1.
func conversationRowJSON(ctx context.Context, db queryer, seq int64) (rowJSON, source, sourceRef string, err error) {
	var platform, title, parentSourceRef string
	var isGroup bool
	var createdAt, updatedAt int64
	if err := db.QueryRowContext(ctx, `SELECT platform, source, source_ref, title, is_group, parent_source_ref, created_at, updated_at FROM conversations WHERE seq = $1`, seq).
		Scan(&platform, &source, &sourceRef, &title, &isGroup, &parentSourceRef, &createdAt, &updatedAt); err != nil {
		return "", "", "", err
	}
	rows, err := db.QueryContext(ctx, `SELECT handle, display_name, person_id, is_self FROM participants WHERE conversation_seq = $1 ORDER BY handle`, seq)
	if err != nil {
		return "", "", "", err
	}
	defer rows.Close()
	participants := []string{}
	for rows.Next() {
		var handle, displayName, personID string
		var isSelf bool
		if err := rows.Scan(&handle, &displayName, &personID, &isSelf); err != nil {
			return "", "", "", err
		}
		var participant orderedJSONObject
		participant.add("handle", handle)
		participant.add("display_name", displayName)
		participant.add("person_id", personID)
		participant.add("is_self", boolInt(isSelf))
		encoded, err := participant.String()
		if err != nil {
			return "", "", "", err
		}
		participants = append(participants, encoded)
	}
	if err := rows.Err(); err != nil {
		return "", "", "", err
	}
	var object orderedJSONObject
	object.add("seq", seq)
	object.add("id", postgresstorage.FormatRecordID(ConversationIDPrefix, seq))
	object.add("platform", platform)
	object.add("source", source)
	object.add("source_ref", sourceRef)
	object.add("title", title)
	object.add("is_group", boolInt(isGroup))
	object.add("parent_source_ref", parentSourceRef)
	object.add("created_at", createdAt)
	object.add("updated_at", updatedAt)
	participantsJSON := "["
	for index, participant := range participants {
		if index > 0 {
			participantsJSON += ","
		}
		participantsJSON += participant
	}
	object.addRaw("participants", participantsJSON+"]")
	rowJSON, err = object.String()
	return rowJSON, source, sourceRef, err
}

// ErrMerged wraps the id of the conversation a merged one now lives in.
type ErrMerged struct{ ID, MergedInto string }

func (e ErrMerged) Error() string {
	return fmt.Sprintf("conversation %s was merged into %s", e.ID, e.MergedInto)
}

const mergeColumns = `conversation_seq, owner_principal_id, source, source_ref, merged_into_seq, rule, import_seq, merged_at, messages_moved, row_json`

// ListConversationMerges answers GET /conversation-merges in scope, newest
// first; mergedInto narrows it to the conversations merged into one.
func (s *Store) ListConversationMerges(ctx context.Context, scope ownership.Scope, mergedInto string, limit int) ([]ConversationMerge, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + mergeColumns + ` FROM conversation_merges WHERE ` + condition
	if mergedInto != "" {
		intoSeq, err := postgresstorage.ParseRecordID(ConversationIDPrefix, mergedInto)
		if err != nil {
			// A malformed id names no conversation, so nothing merged into it.
			return []ConversationMerge{}, nil
		}
		query += ` AND merged_into_seq = ` + args.add(intoSeq)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY merged_at DESC, conversation_seq LIMIT `+args.add(limit), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConversationMerge{}
	for rows.Next() {
		var merge ConversationMerge
		var conversationSeq, intoSeq, importSeq int64
		var rowJSON string
		if err := rows.Scan(&conversationSeq, &merge.OwnerPrincipalID, &merge.Source, &merge.SourceRef, &intoSeq, &merge.Rule, &importSeq,
			&merge.MergedAt, &merge.MessagesMoved, &rowJSON); err != nil {
			return nil, err
		}
		merge.ConversationID = postgresstorage.FormatRecordID(ConversationIDPrefix, conversationSeq)
		merge.MergedInto = postgresstorage.FormatRecordID(ConversationIDPrefix, intoSeq)
		merge.ImportID = postgresstorage.FormatRecordID(ImportIDPrefix, importSeq)
		merge.Row = []byte(rowJSON)
		out = append(out, merge)
	}
	return out, rows.Err()
}

// mergedIntoSeq is the conversation a merged conversation number now lives
// in, among the records in scope; 0 when it was never merged there.
func (s *Store) mergedIntoSeq(ctx context.Context, scope ownership.Scope, seq int64) (int64, error) {
	args := &arguments{}
	condition, err := args.scope(scope, "owner_principal_id")
	if err != nil {
		return 0, err
	}
	var into int64
	err = s.db.QueryRowContext(ctx, `SELECT merged_into_seq FROM conversation_merges WHERE conversation_seq = `+args.add(seq)+` AND `+condition, args.values...).Scan(&into)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return into, err
}

// liveConversationSeqs reads conversation ids as numbers and replaces each
// merged one by the one it merged into, so a filter naming an old id still
// finds its messages. A malformed id names no conversation and is dropped.
func (s *Store) liveConversationSeqs(ctx context.Context, scope ownership.Scope, ids []string) ([]int64, error) {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		seq, err := postgresstorage.ParseRecordID(ConversationIDPrefix, id)
		if err != nil {
			continue
		}
		into, err := s.mergedIntoSeq(ctx, scope, seq)
		if err != nil {
			return nil, err
		}
		if into != 0 {
			seq = into
		}
		out = append(out, seq)
	}
	return out, nil
}
