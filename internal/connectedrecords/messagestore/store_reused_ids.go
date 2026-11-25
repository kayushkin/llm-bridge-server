package messagestore

import (
	"database/sql"
	"fmt"
	"log"
)

// Until nextSeq counted retired ids, a conversation filed right after the
// newest conversation was merged got the merged one's id again. A read by that
// id follows the merge's redirect, so the live conversation answered with the
// other conversation's messages. repairReusedConversationIDs gives each such
// live conversation a fresh id, moving its participants, messages and days with
// it, and leaves the merge's redirect as it was. It runs at every start and
// changes nothing once no live id is also a merged one.
//
// A live message holding a superseded message's id has the same fault. None
// was ever made, so that case is refused loudly rather than repaired.
func repairReusedConversationIDs(db *sql.DB) error {
	var reusedMessages int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id IN (SELECT id FROM superseded_messages)`).Scan(&reusedMessages); err != nil {
		return err
	}
	if reusedMessages > 0 {
		return fmt.Errorf("%d live messages hold the id of a superseded message; nothing repairs that yet", reusedMessages)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The conversation's id and its children's references change together;
	// the keys are checked at commit.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT c.id, m.merged_into FROM conversations c JOIN conversation_merges m ON m.conversation_id = c.id ORDER BY c.seq`)
	if err != nil {
		return err
	}
	type reused struct{ id, mergedInto string }
	var found []reused
	for rows.Next() {
		var r reused
		if err := rows.Scan(&r.id, &r.mergedInto); err != nil {
			rows.Close()
			return err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}
	for _, r := range found {
		seq, err := nextSeq(tx, "conversations")
		if err != nil {
			return err
		}
		freshID := formatID(prefixConversation, seq)
		if _, err := tx.Exec(`UPDATE conversations SET seq = ?, id = ? WHERE id = ?`, seq, freshID, r.id); err != nil {
			return fmt.Errorf("give conversation %s the id %s: %w", r.id, freshID, err)
		}
		for _, table := range []string{"participants", "messages", "conversation_days"} {
			if _, err := tx.Exec(`UPDATE `+table+` SET conversation_id = ? WHERE conversation_id = ?`, freshID, r.id); err != nil {
				return fmt.Errorf("move %s of conversation %s to %s: %w", table, r.id, freshID, err)
			}
		}
		log.Printf("repaired a reused id: the conversation filed as %s is now %s; %s stays the merge into %s", r.id, freshID, r.id, r.mergedInto)
	}
	return tx.Commit()
}
